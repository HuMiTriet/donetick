package beeminder

import (
	"donetick.com/core/config"
	auth "donetick.com/core/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/ulule/limiter/v3"
)

type Handler struct {
}

func Routes(router *gin.Engine, h *Handler, multiAuthMiddleware *auth.MultiAuthMiddleware, limiter *limiter.Limiter, cfg *config.Config) {

}
