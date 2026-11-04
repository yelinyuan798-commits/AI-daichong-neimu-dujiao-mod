// Package application exposes only paid manual-order fulfillment to an authenticated merchant connector.
package application

import (
	"errors"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

const Protocol = "uecb-fulfillment-v1"

var (
	ErrNotFound = errors.New("order not found")
	ErrBlocked  = errors.New("order is not eligible for merchant fulfillment")
	ErrConflict = errors.New("delivery already exists or request changed")
	ErrInvalid  = errors.New("invalid delivery request")
)

// Reader never returns customer credentials or stock to the HTTP layer.
type Reader interface {
	FindIDByOrderNo(string) (uint, error)
	ListCandidates(after uint, limit int) ([]orderdomain.Order, error)
}

type Item struct {
	LineID   string `json:"line_id"`
	ItemID   string `json:"item_id"`
	SKUID    string `json:"sku_id"`
	Quantity int    `json:"quantity"`
	Amount   string `json:"amount"`
}

type Order struct {
	Platform       string `json:"platform"`
	ShopID         string `json:"shop_id"`
	OrderID        string `json:"order_id"`
	BuyerID        string `json:"buyer_id"`
	Status         string `json:"status"`
	Amount         string `json:"amount"`
	Currency       string `json:"currency"`
	ShippingAmount string `json:"shipping_amount"`
	Items          []Item `json:"items"`
}

type OrderRef struct {
	OrderID string `json:"order_id"`
}
type Page struct {
	Orders     []OrderRef `json:"orders"`
	NextCursor *string    `json:"next_cursor"`
}

type Payload struct {
	Type           string   `json:"type"`
	Cards          []string `json:"cards,omitempty"`
	Content        string   `json:"content,omitempty"`
	Carrier        string   `json:"carrier,omitempty"`
	TrackingNumber string   `json:"tracking_number,omitempty"`
}

type Delivery struct {
	IdempotencyKey string  `json:"idempotency_key"`
	OrderID        string  `json:"order_id"`
	LineID         string  `json:"line_id"`
	BuyerID        string  `json:"buyer_id"`
	ItemID         string  `json:"item_id"`
	SKUID          string  `json:"sku_id"`
	Quantity       int     `json:"quantity"`
	Amount         string  `json:"amount"`
	Currency       string  `json:"currency"`
	Payload        Payload `json:"payload"`
}

type Receipt struct {
	Status  string `json:"status"`
	Receipt string `json:"receipt"`
}
