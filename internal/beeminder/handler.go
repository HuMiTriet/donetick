package beeminder

import (
	"errors"
	"net/http"

	"donetick.com/core/config"
	auth "donetick.com/core/internal/auth"
	"donetick.com/core/internal/utils"
	"donetick.com/core/logging"
	"github.com/gin-gonic/gin"
	"github.com/ulule/limiter/v3"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// accountResponse is what the settings page and the task editors read.
// Username is the linked Beeminder username. TokenValid is only set when the
// request checked the token with Beeminder (?live=true, or just linked it).
// DryRun is read-only: the server decides it (see Service.DryRun).
type accountResponse struct {
	Username          *string `json:"username"`
	TokenValid        *bool   `json:"tokenValid"`
	Deadbeat          bool    `json:"deadbeat"`
	DryRun            bool    `json:"dryRun"`
	DefaultLabelID    *int    `json:"defaultLabelId"`
	EditWindowSeconds int     `json:"editWindowSeconds"`
	MinAmountCents    int     `json:"minAmountCents"`
	MaxAmountCents    int     `json:"maxAmountCents"`
}

func (h *Handler) toResponse(account *BeeminderAccount, tokenValid *bool) accountResponse {
	return accountResponse{
		Username:          account.Username,
		TokenValid:        tokenValid,
		Deadbeat:          account.Deadbeat,
		DryRun:            h.svc.DryRun(account),
		DefaultLabelID:    account.DefaultLabelID,
		EditWindowSeconds: int(h.svc.EditWindow().Seconds()),
		MinAmountCents:    MinAmountCents,
		MaxAmountCents:    h.svc.MaxAmountCents(),
	}
}

func (h *Handler) getAccount(c *gin.Context) {
	logger := logging.FromContext(c)
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication failed"})
		return
	}
	account, err := h.svc.GetAccount(c, user.ID)
	if err != nil {
		logger.Errorw("Failed to load Beeminder account", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load Beeminder account"})
		return
	}
	if account == nil {
		c.JSON(http.StatusOK, gin.H{"res": nil})
		return
	}

	var tokenValid *bool
	if c.Query("live") == "true" {
		// Also refreshes the stored deadbeat flag.
		_, err := h.svc.BeeminderUser(c, account)
		switch {
		case err == nil:
			valid := true
			tokenValid = &valid
		case errors.Is(err, ErrInvalidToken):
			valid := false
			tokenValid = &valid
		default:
			logger.Warnw("Couldn't reach Beeminder", "error", err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"res": h.toResponse(account, tokenValid)})
}

type linkAccountReq struct {
	AuthToken string `json:"authToken" binding:"required"`
}

func (h *Handler) linkAccount(c *gin.Context) {
	logger := logging.FromContext(c)
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication failed"})
		return
	}
	var req linkAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter your Beeminder auth token"})
		return
	}

	account, _, err := h.svc.LinkAccount(c, &user.User, req.AuthToken)
	if err != nil {
		var validation *ValidationError
		switch {
		case errors.Is(err, ErrInvalidToken):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Beeminder rejected this auth token"})
		case errors.As(err, &validation):
			c.JSON(http.StatusBadRequest, gin.H{"error": validation.Error()})
		default:
			logger.Errorw("Failed to link Beeminder account", "error", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "Couldn't reach Beeminder"})
		}
		return
	}
	valid := true // Beeminder just accepted the token
	c.JSON(http.StatusOK, gin.H{"res": h.toResponse(account, &valid)})
}

// settingsReq holds what a user may change. Dry run isn't here on purpose:
// only the server decides it.
type settingsReq struct {
	DefaultLabelID *int `json:"defaultLabelId"`
}

func (h *Handler) updateSettings(c *gin.Context) {
	logger := logging.FromContext(c)
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication failed"})
		return
	}
	var req settingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	account, err := h.svc.UpdateSettings(c, &user.User, req.DefaultLabelID)
	if err != nil {
		status := StatusCode(err)
		if status == http.StatusInternalServerError {
			logger.Errorw("Failed to update Beeminder settings", "error", err)
			c.JSON(status, gin.H{"error": "Failed to save Beeminder settings"})
			return
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"res": h.toResponse(account, nil)})
}

func Routes(router *gin.Engine, h *Handler, multiAuthMiddleware *auth.MultiAuthMiddleware, limiter *limiter.Limiter, cfg *config.Config) {
	if !cfg.Beeminder.Enabled {
		return
	}
	routes := router.Group("api/v1/beeminder")
	routes.Use(multiAuthMiddleware.MiddlewareFunc(), utils.RateLimitMiddleware(limiter))
	{
		routes.GET("/account", h.getAccount)
		routes.PUT("/account", h.linkAccount)
		routes.PUT("/account/settings", h.updateSettings)
	}
}
