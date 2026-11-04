package merchantfulfillmentwiring

import (
	"errors"
	"github.com/dujiao-next/internal/app/container"
	merchantapp "github.com/dujiao-next/internal/modules/merchantfulfillment/application"
	merchantstore "github.com/dujiao-next/internal/modules/merchantfulfillment/infrastructure/gormstore"
	merchanthttp "github.com/dujiao-next/internal/modules/merchantfulfillment/transport/http"
	"github.com/dujiao-next/internal/platform/database/gormdb"
	"github.com/gin-gonic/gin"
)

// Register is deliberately disabled unless both independent bridge settings are present.
func Register(api *gin.RouterGroup, c *container.Container) error {
	cfg, err := merchanthttp.ConfigFromEnv()
	if err != nil {
		return err
	}
	if cfg.Secret == "" {
		return nil
	}
	if c == nil || c.OrderStore == nil || gormdb.DB == nil {
		return errors.New("merchant fulfillment storage unavailable")
	}
	merchanthttp.RegisterRoutes(api, merchantapp.New(c.OrderStore, merchantstore.New(gormdb.DB), cfg.ShopID), cfg)
	return nil
}
