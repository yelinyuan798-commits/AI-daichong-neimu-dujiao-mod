package merchanthttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	merchantapp "github.com/dujiao-next/internal/modules/merchantfulfillment/application"
	"github.com/gin-gonic/gin"
)

type Service interface {
	List(uint, int) (merchantapp.Page, error)
	Get(string) (merchantapp.Order, error)
	Deliver(merchantapp.Delivery) (merchantapp.Receipt, error)
}

func RegisterRoutes(group *gin.RouterGroup, service Service, cfg Config) {
	if len([]byte(cfg.Secret)) < 32 || !identifier.MatchString(cfg.ShopID) || strings.Contains(cfg.ShopID, cfg.Secret) || service == nil {
		panic("invalid merchant fulfillment dependencies")
	}
	routes := group.Group("/merchant-fulfillment")
	routes.Use(newAuthenticator(cfg.Secret).middleware)
	routes.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "protocol": merchantapp.Protocol, "platform": "dujiao", "shop_id": cfg.ShopID, "capabilities": []string{"card", "link", "physical"}})
	})
	routes.GET("/orders", func(c *gin.Context) {
		cursor := uint64(0)
		var err error
		if value := c.Query("cursor"); value != "" {
			cursor, err = strconv.ParseUint(value, 10, 32)
		}
		if err != nil {
			respondError(c, merchantapp.ErrInvalid)
			return
		}
		limit := 50
		if value := c.Query("limit"); value != "" {
			limit, err = strconv.Atoi(value)
		}
		if err != nil || limit < 1 || limit > 50 {
			respondError(c, merchantapp.ErrInvalid)
			return
		}
		page, err := service.List(uint(cursor), limit)
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	})
	routes.GET("/orders/:order_id", func(c *gin.Context) {
		order, err := service.Get(c.Param("order_id"))
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(http.StatusOK, order)
	})
	routes.POST("/deliveries", func(c *gin.Context) {
		var req merchantapp.Delivery
		decoder := json.NewDecoder(c.Request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			respondError(c, merchantapp.ErrInvalid)
			return
		}
		if err := decoder.Decode(new(interface{})); err != io.EOF {
			respondError(c, merchantapp.ErrInvalid)
			return
		}
		receipt, err := service.Deliver(req)
		if err != nil {
			if errors.Is(err, merchantapp.ErrNotFound) || errors.Is(err, merchantapp.ErrInvalid) || errors.Is(err, merchantapp.ErrBlocked) || errors.Is(err, merchantapp.ErrConflict) {
				c.JSON(http.StatusOK, gin.H{"status": "failed", "error": "order or delivery request was rejected"})
				return
			}
			respondError(c, err)
			return
		}
		c.JSON(http.StatusOK, receipt)
	})
}

// Database/commit errors are ambiguous; clients must query/replay the same key, never allocate new stock.
func respondError(c *gin.Context, err error) {
	code, status, message := http.StatusInternalServerError, "unknown", "delivery result requires verification"
	switch {
	case errors.Is(err, merchantapp.ErrNotFound):
		code, status, message = http.StatusNotFound, "failed", "order not found"
	case errors.Is(err, merchantapp.ErrInvalid):
		code, status, message = http.StatusBadRequest, "failed", "invalid request"
	case errors.Is(err, merchantapp.ErrBlocked):
		code, status, message = http.StatusConflict, "failed", "order is not eligible"
	case errors.Is(err, merchantapp.ErrConflict):
		code, status, message = http.StatusConflict, "failed", "delivery conflict"
	}
	c.JSON(code, gin.H{"status": status, "error": message})
}
