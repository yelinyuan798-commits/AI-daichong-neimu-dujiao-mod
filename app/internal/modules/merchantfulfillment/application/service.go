package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/shopspring/decimal"
)

type Service struct {
	orders ordercontract.Store
	reader Reader
	ShopID string
}

func New(orders ordercontract.Store, reader Reader, shopID string) *Service {
	return &Service{orders: orders, reader: reader, ShopID: shopID}
}

func (s *Service) List(after uint, limit int) (Page, error) {
	page := Page{Orders: []OrderRef{}}
	if limit < 1 || limit > 50 {
		return page, ErrInvalid
	}
	rows, err := s.reader.ListCandidates(after, limit+1)
	if err != nil {
		return page, err
	}
	if len(rows) > limit {
		cursor := strconv.FormatUint(uint64(rows[limit-1].ID), 10)
		page.NextCursor = &cursor
		rows = rows[:limit]
	}
	for _, row := range rows {
		order, err := s.Get(row.OrderNo)
		if err == ErrBlocked || err == ErrNotFound {
			continue
		}
		if err != nil {
			return page, err
		}
		if order.Status == "paid" {
			page.Orders = append(page.Orders, OrderRef{OrderID: order.OrderID})
		}
	}
	return page, nil
}

// withOrder uses the same root-then-child lock order as the refund workflow.
func (s *Service) withOrder(orderNo string, fn func(ordercontract.Transaction, *orderdomain.Order, *orderdomain.Order) error) error {
	if orderNo == "" || len(orderNo) > 100 {
		return ErrInvalid
	}
	id, err := s.reader.FindIDByOrderNo(orderNo)
	if err != nil {
		return err
	}
	if id == 0 {
		return ErrNotFound
	}
	initial, err := s.orders.GetByID(id)
	if err != nil {
		return err
	}
	if initial == nil {
		return ErrNotFound
	}
	rootID := id
	if initial.ParentID != nil {
		rootID = *initial.ParentID
	}
	return s.orders.WithinTransaction(func(tx ordercontract.Transaction) error {
		root, err := tx.Orders().GetByIDForUpdateWithChildren(rootID)
		if err != nil {
			return err
		}
		if root == nil || root.DeletedAt != nil || root.ParentID != nil || root.ResellerID != nil {
			return ErrNotFound
		}
		order := root
		if rootID != id {
			order, err = tx.Orders().GetByIDForUpdateWithChildren(id)
			if err != nil {
				return err
			}
			if order == nil || order.ParentID == nil || *order.ParentID != rootID {
				return ErrNotFound
			}
		}
		if order.DeletedAt != nil || order.OrderNo != orderNo || order.ResellerID != nil {
			return ErrNotFound
		}
		return fn(tx, root, order)
	})
}

func (s *Service) canonical(tx ordercontract.Transaction, root, order *orderdomain.Order) (Order, error) {
	result := Order{}
	if len(order.Children) > 0 || len(order.Items) != 1 {
		return result, ErrBlocked
	}
	item := order.Items[0]
	if item.DeletedAt != nil || item.FulfillmentType != constants.FulfillmentTypeManual || item.Quantity <= 0 || item.ProductID == 0 || item.ID == 0 {
		return result, ErrBlocked
	}
	if root.UserID != order.UserID || root.Currency != order.Currency || order.TotalAmount.Decimal.IsNegative() {
		return result, ErrBlocked
	}
	buyer := "user:" + strconv.FormatUint(uint64(root.UserID), 10)
	if root.UserID == 0 {
		buyer = "guest:" + strconv.FormatUint(uint64(root.ID), 10)
	}
	status := "unpaid"
	ids := []uint{root.ID}
	for _, child := range root.Children {
		ids = append(ids, child.ID)
	}
	refunds, err := tx.Orders().ListRefundRecordsByOrderIDs(ids)
	if err != nil {
		return result, err
	}
	refunded := root.RefundedAmount.Decimal.IsPositive() || order.RefundedAmount.Decimal.IsPositive() || len(refunds) > 0 || root.Status == constants.OrderStatusRefunded || root.Status == constants.OrderStatusPartiallyRefunded || order.Status == constants.OrderStatusRefunded || order.Status == constants.OrderStatusPartiallyRefunded
	fulfillment, _, err := tx.Fulfillments().FindByOrderIDForUpdate(order.ID)
	if err != nil {
		return result, err
	}
	switch {
	case refunded:
		status = "refunded"
	case root.Status == constants.OrderStatusCanceled || order.Status == constants.OrderStatusCanceled || root.CanceledAt != nil || order.CanceledAt != nil:
		status = "cancelled"
	case fulfillment != nil || order.Status == constants.OrderStatusDelivered || order.Status == constants.OrderStatusCompleted:
		status = "fulfilled"
	case root.PaidAt != nil && order.PaidAt != nil && (root.Status == constants.OrderStatusPaid || root.Status == constants.OrderStatusFulfilling || root.Status == constants.OrderStatusPartiallyDelivered) && (order.Status == constants.OrderStatusPaid || order.Status == constants.OrderStatusFulfilling):
		if !root.WalletPaidAmount.Decimal.Add(root.OnlinePaidAmount.Decimal).Equal(root.TotalAmount.Decimal) {
			return result, ErrBlocked
		}
		if len(root.Children) > 0 {
			sum := decimal.Zero
			for _, child := range root.Children {
				sum = sum.Add(child.TotalAmount.Decimal)
			}
			if !sum.Equal(root.TotalAmount.Decimal) {
				return result, ErrBlocked
			}
		}
		status = "paid"
	}
	sku := ""
	if item.SKUID > 0 {
		sku = strconv.FormatUint(uint64(item.SKUID), 10)
	}
	return Order{Platform: "dujiao", ShopID: s.ShopID, OrderID: order.OrderNo, BuyerID: buyer, Status: status, Amount: order.TotalAmount.StringFixed(2), Currency: order.Currency, ShippingAmount: "0.00", Items: []Item{{LineID: strconv.FormatUint(uint64(item.ID), 10), ItemID: strconv.FormatUint(uint64(item.ProductID), 10), SKUID: sku, Quantity: item.Quantity, Amount: order.TotalAmount.StringFixed(2)}}}, nil
}

func (s *Service) Get(orderNo string) (Order, error) {
	var result Order
	err := s.withOrder(orderNo, func(tx ordercontract.Transaction, root, order *orderdomain.Order) error {
		var err error
		result, err = s.canonical(tx, root, order)
		return err
	})
	return result, err
}

func digest(value []byte) string { hash := sha256.Sum256(value); return hex.EncodeToString(hash[:]) }

func safeText(value string, max int, multiline bool) bool {
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > max {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\r' || r == '\t')) {
			return false
		}
	}
	return true
}

func deliveryText(req Delivery) (string, jsonmap.JSON, error) {
	p := req.Payload
	data := jsonmap.JSON{}
	switch p.Type {
	case "card":
		if len(p.Cards) != req.Quantity || p.Content != "" || p.Carrier != "" || p.TrackingNumber != "" {
			return "", nil, ErrInvalid
		}
		for _, card := range p.Cards {
			if !safeText(card, 10000, false) {
				return "", nil, ErrInvalid
			}
		}
		return strings.Join(p.Cards, "\n"), data, nil
	case "link":
		if !safeText(p.Content, 200000, true) || len(p.Cards) != 0 || p.Carrier != "" || p.TrackingNumber != "" {
			return "", nil, ErrInvalid
		}
		return p.Content, data, nil
	case "physical":
		if !safeText(p.Carrier, 100, false) || !safeText(p.TrackingNumber, 128, false) || len(p.Cards) != 0 || p.Content != "" {
			return "", nil, ErrInvalid
		}
		data["entries"] = []jsonmap.JSON{{"key": "物流公司", "value": p.Carrier}, {"key": "运单号", "value": p.TrackingNumber}}
		return "物流公司: " + p.Carrier + "\n运单号: " + p.TrackingNumber, data, nil
	default:
		return "", nil, ErrInvalid
	}
}

func (s *Service) Deliver(req Delivery) (Receipt, error) {
	if !strings.HasPrefix(req.IdempotencyKey, "fulfillment:") || !safeText(req.IdempotencyKey, 160, false) || req.Quantity < 1 || req.Quantity > 10000 {
		return Receipt{}, ErrInvalid
	}
	text, data, err := deliveryText(req)
	if err != nil {
		return Receipt{}, err
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.IsNegative() || !amount.Equal(amount.Round(2)) {
		return Receipt{}, ErrInvalid
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return Receipt{}, ErrInvalid
	}
	requestHash, keyHash := digest(encoded), digest([]byte(req.IdempotencyKey))
	var result Receipt
	err = s.withOrder(req.OrderID, func(tx ordercontract.Transaction, root, order *orderdomain.Order) error {
		existing, found, err := tx.Fulfillments().FindByOrderIDForUpdate(order.ID)
		if err != nil {
			return err
		}
		if found {
			if existing.LogisticsJSON["merchant_bridge_protocol"] != Protocol || existing.LogisticsJSON["merchant_bridge_key"] != keyHash || existing.LogisticsJSON["merchant_bridge_request"] != requestHash {
				return ErrConflict
			}
			result = Receipt{Status: "sent", Receipt: fmt.Sprintf("dujiao:%s:%d", s.ShopID, existing.ID)}
			return nil
		}
		canonical, err := s.canonical(tx, root, order)
		if err != nil {
			return err
		}
		if canonical.Status != "paid" {
			return ErrBlocked
		}
		item := canonical.Items[0]
		if req.BuyerID != canonical.BuyerID || req.LineID != item.LineID || req.ItemID != item.ItemID || req.SKUID != item.SKUID || req.Quantity != item.Quantity || req.Currency != canonical.Currency || !amount.Equal(order.TotalAmount.Decimal) {
			return ErrConflict
		}
		now := time.Now()
		data["merchant_bridge_protocol"] = Protocol
		data["merchant_bridge_key"] = keyHash
		data["merchant_bridge_request"] = requestHash
		created := &fulfillmentdomain.Fulfillment{OrderID: order.ID, Type: constants.FulfillmentTypeManual, Status: constants.FulfillmentStatusDelivered, Payload: text, LogisticsJSON: data, DeliveredAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := tx.Fulfillments().Create(created); err != nil {
			return err
		}
		if err := tx.Orders().UpdateFields(order.ID, map[string]interface{}{"status": constants.OrderStatusDelivered, "updated_at": now}); err != nil {
			return err
		}
		if order.ParentID != nil {
			if _, err := orderapp.SyncParentStatus(tx.Orders(), root.ID, now); err != nil {
				return err
			}
		}
		result = Receipt{Status: "sent", Receipt: fmt.Sprintf("dujiao:%s:%d", s.ShopID, created.ID)}
		return nil
	})
	return result, err
}
