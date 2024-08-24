/* The /auth group contains all the routes to sign in and sign out */
package routes

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"schej.it/server/db"
	"schej.it/server/logger"
	"schej.it/server/middleware"
	"schej.it/server/models"
	"schej.it/server/services/auth"
	"schej.it/server/services/calendar"
	"schej.it/server/services/listmonk"
	"schej.it/server/slackbot"
	"schej.it/server/utils"
)

func InitAuth(router *gin.RouterGroup) {
	authRouter := router.Group("/auth")

	authRouter.POST("/sign-in", signIn)
	authRouter.POST("/sign-in-mobile", signInMobile)
	authRouter.POST("/sign-out", signOut)
	authRouter.GET("/status", middleware.AuthRequired(), getStatus)
}

// @Summary Signs user in
// @Description Signs user in and sets the access token session variable
// @Tags auth
// @Accept json
// @Produce json
// @Param payload body object{code=string,scope=string,timezoneOffset=int} true "Object containing the Google authorization code, scope, and the user's timezone offset"
// @Success 200
// @Router /auth/sign-in [post]
func signIn(c *gin.Context) {
	payload := struct {
		Code           string `json:"code" binding:"required"`
		Scope          string `json:"scope" binding:"required"`
		TimezoneOffset int    `json:"timezoneOffset" binding:"required"`
	}{}
	if err := c.BindJSON(&payload); err != nil {
		return
	}

	tokens := auth.GetTokensFromAuthCode(payload.Code, payload.Scope, utils.GetOrigin(c), models.GoogleCalendarType)

	signInHelper(c, tokens, models.WEB, payload.TimezoneOffset)

	c.JSON(http.StatusOK, gin.H{})
}

// @Summary Signs user in from mobile
// @Description Signs user in and sets the access token session variable
// @Tags auth
// @Accept json
// @Produce json
// @Param payload body object{timezoneOffset=int,accessToken=string,scope=string,idToken=string,expiresIn=int,refreshToken=string,tokenOrigin=string} true "Object containing the Google authorization code and the user's timezone offset"
// @Success 200
// @Router /auth/sign-in-mobile [post]
func signInMobile(c *gin.Context) {
	payload := struct {
		TimezoneOffset int                    `json:"timezoneOffset" binding:"required"`
		AccessToken    string                 `json:"accessToken" binding:"required"`
		Scope          string                 `json:"scope" binding:"required"`
		IdToken        string                 `json:"idToken" binding:"required"`
		ExpiresIn      int                    `json:"expiresIn" binding:"required"`
		RefreshToken   string                 `json:"refreshToken" binding:"required"`
		TokenOrigin    models.TokenOriginType `json:"tokenOrigin" binding:"required"`
	}{}
	if err := c.BindJSON(&payload); err != nil {
		return
	}

	signInHelper(c, auth.TokenResponse{AccessToken: payload.AccessToken, IdToken: payload.IdToken, ExpiresIn: payload.ExpiresIn, RefreshToken: payload.RefreshToken, Scope: payload.Scope}, payload.TokenOrigin, payload.TimezoneOffset)

	c.JSON(http.StatusOK, gin.H{})
}

// Helper function to sign user in with the given parameters from the google oauth route
func signInHelper(c *gin.Context, token auth.TokenResponse, tokenOrigin models.TokenOriginType, timezoneOffset int) {
	// Get access token expire time
	accessTokenExpireDate := utils.GetAccessTokenExpireDate(token.ExpiresIn)

	// Get user info from JWT
	claims := utils.ParseJWT(token.IdToken)
	email, _ := claims.GetStr("email")
	firstName, _ := claims.GetStr("given_name")
	lastName, _ := claims.GetStr("family_name")
	picture, _ := claims.GetStr("picture")

	// Create user object to create new user or update existing user
	userData := models.User{
		Email:     email,
		FirstName: firstName,
		LastName:  lastName,
		Picture:   picture,

		TimezoneOffset: timezoneOffset,
		TokenOrigin:    tokenOrigin,
	}

	calendarAccount := models.CalendarAccount{
		CalendarType: models.GoogleCalendarType,
		OAuth2CalendarAuth: &models.OAuth2CalendarAuth{
			AccessToken:           token.AccessToken,
			AccessTokenExpireDate: primitive.NewDateTimeFromTime(accessTokenExpireDate),
			RefreshToken:          token.RefreshToken,
			Scope:                 token.Scope,
		},

		Email:   email,
		Picture: picture,
		Enabled: utils.TruePtr(), // Workaround to pass a boolean pointer
	}
	calendarAccountKey := utils.GetCalendarAccountKey(email, models.GoogleCalendarType)

	var userId primitive.ObjectID
	findResult := db.UsersCollection.FindOne(context.Background(), bson.M{"email": email})
	// If user doesn't exist, create a new user
	if findResult.Err() == mongo.ErrNoDocuments {
		// Fetch subcalendars
		subCalendars, err := calendar.GetCalendarProvider(calendarAccount).GetCalendarList()
		if err == nil {
			calendarAccount.SubCalendars = &subCalendars
		}

		// Set calendar accounts
		userData.CalendarAccounts = map[string]models.CalendarAccount{
			calendarAccountKey: calendarAccount,
		}

		// Create user
		res, err := db.UsersCollection.InsertOne(context.Background(), userData)
		if err != nil {
			logger.StdErr.Panicln(err)
		}

		userId = res.InsertedID.(primitive.ObjectID)

		slackbot.SendTextMessage(fmt.Sprintf(":wave: %s %s (%s) has joined schej.it!", firstName, lastName, email))
	} else {
		var user models.User
		if err := findResult.Decode(&user); err != nil {
			logger.StdErr.Panicln(err)
		}
		userId = user.Id

		// If user has custom name, do not override first name and last name
		if user.HasCustomName != nil && *user.HasCustomName {
			userData.FirstName = ""
			userData.LastName = ""
		}

		// Set subcalendars map based on whether calendar account already exists
		if oldCalendarAccount, ok := user.CalendarAccounts[calendarAccountKey]; ok && oldCalendarAccount.SubCalendars != nil {
			calendarAccount.SubCalendars = oldCalendarAccount.SubCalendars
		} else {
			subCalendars, err := calendar.GetCalendarProvider(calendarAccount).GetCalendarList()
			if err == nil {
				calendarAccount.SubCalendars = &subCalendars
			}
		}

		// Set calendar account
		userData.CalendarAccounts = user.CalendarAccounts
		userData.CalendarAccounts[calendarAccountKey] = calendarAccount

		// Update user if exists
		_, err := db.UsersCollection.UpdateByID(
			context.Background(),
			userId,
			bson.M{"$set": userData},
		)
		if err != nil {
			logger.StdErr.Panicln(err)
		}
	}

	if exists, userId := listmonk.DoesUserExist(email); exists {
		listmonk.AddUserToListmonk(email, firstName, lastName, picture, userId)
	} else {
		listmonk.AddUserToListmonk(email, firstName, lastName, picture, nil)
	}

	// Set session variables
	session := sessions.Default(c)
	session.Set("userId", userId.Hex())
	session.Save()
}

// @Summary Signs user out
// @Description Signs user out and deletes the session
// @Tags auth
// @Accept json
// @Produce json
// @Success 200
// @Router /auth/sign-out [post]
func signOut(c *gin.Context) {
	// Delete session
	session := sessions.Default(c)
	session.Delete("userId")
	session.Save()

	c.JSON(http.StatusOK, gin.H{})
}

// @Summary Gets whether the user is signed in or not
// @Description Returns a 401 error if user is not signed in, 200 if they are
// @Tags auth
// @Success 200
// @Failure 401 {object} responses.Error "Error object"
// @Router /auth/status [get]
func getStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{})
}
