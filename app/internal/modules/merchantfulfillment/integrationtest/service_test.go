package merchantfulfillment_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	merchantapp "github.com/dujiao-next/internal/modules/merchantfulfillment/application"
	merchantstore "github.com/dujiao-next/internal/modules/merchantfulfillment/infrastructure/gormstore"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	db      *gorm.DB
	orders  ordercontract.Store
	service *merchantapp.Service
	root    orderdomain.Order
	child   orderdomain.Order
	item    orderdomain.OrderItem
}

func amount(value string) money.Amount { return money.FromDecimal(decimal.RequireFromString(value)) }

func setup(t *testing.T) *fixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bridge.db")), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&orderdomain.Order{}, &orderdomain.OrderItem{}, &orderdomain.OrderRefundRecord{}, &fulfillmentdomain.Fulfillment{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := orderdomain.Order{OrderNo: "PUBLIC-TEST-PARENT", UserID: 7, Status: constants.OrderStatusFulfilling, Currency: "CNY", TotalAmount: amount("15.00"), OnlinePaidAmount: amount("15.00"), PaidAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&root).Error; err != nil {
		t.Fatal(err)
	}
	child := orderdomain.Order{OrderNo: "PUBLIC-TEST-LEAF", ParentID: &root.ID, UserID: 7, Status: constants.OrderStatusFulfilling, Currency: "CNY", TotalAmount: amount("6.00"), OnlinePaidAmount: amount("6.00"), PaidAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&child).Error; err != nil {
		t.Fatal(err)
	}
	second := orderdomain.Order{OrderNo: "PUBLIC-TEST-SECOND", ParentID: &root.ID, UserID: 7, Status: constants.OrderStatusFulfilling, Currency: "CNY", TotalAmount: amount("9.00"), OnlinePaidAmount: amount("9.00"), PaidAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	item := orderdomain.OrderItem{OrderID: child.ID, ProductID: 42, SKUID: 43, TitleJSON: jsonmap.JSON{"zh-CN": "示例商品"}, Quantity: 2, UnitPrice: amount("5.00"), TotalPrice: amount("10.00"), FulfillmentType: constants.FulfillmentTypeManual, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	secondItem := item
	secondItem.ID = 0
	secondItem.OrderID = second.ID
	if err := db.Create(&secondItem).Error; err != nil {
		t.Fatal(err)
	}
	orders := orderstore.New(db, "public-test-guest-credential-secret-32-bytes")
	return &fixture{db: db, orders: orders, service: merchantapp.New(orders, merchantstore.New(db), "test-shop"), root: root, child: child, item: item}
}

func (f *fixture) request() merchantapp.Delivery {
	return merchantapp.Delivery{IdempotencyKey: "fulfillment:public-test-job", OrderID: f.child.OrderNo, LineID: fmt.Sprint(f.item.ID), BuyerID: "user:7", ItemID: "42", SKUID: "43", Quantity: 2, Amount: "6.00", Currency: "CNY", Payload: merchantapp.Payload{Type: "card", Cards: []string{"PUBLIC-TEST-CARD-A", "PUBLIC-TEST-CARD-B"}}}
}

func countDeliveries(t *testing.T, f *fixture) int64 {
	t.Helper()
	var count int64
	if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCanonicalUsesDiscountedLeafAmountAndPrivateBuyerIdentity(t *testing.T) {
	f := setup(t)
	for _, id := range []uint{f.root.ID, f.child.ID} {
		if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", id).Updates(map[string]interface{}{"user_id": 0, "guest_email": "public-test@example.invalid", "guest_password": "not-a-real-credential"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.service.Get(f.child.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != "6.00" || got.Items[0].Amount != "6.00" || got.Status != "paid" || got.BuyerID != fmt.Sprintf("guest:%d", f.root.ID) {
		t.Fatalf("unexpected canonical order: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "example.invalid") || strings.Contains(string(encoded), "credential") {
		t.Fatal("private customer fields leaked")
	}
}

func TestDeliveryKindsPersistBuyerReadableContentAndReplay(t *testing.T) {
	for _, kind := range []string{"card", "link", "physical"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			req := f.request()
			switch kind {
			case "link":
				req.Payload = merchantapp.Payload{Type: "link", Content: "https://example.invalid/public-test\n提取码：DEMO"}
			case "physical":
				req.Payload = merchantapp.Payload{Type: "physical", Carrier: "测试物流", TrackingNumber: "DEMO-TRACKING-001"}
			}
			first, err := f.service.Deliver(req)
			if err != nil {
				t.Fatal(err)
			}
			if first.Status != "sent" || first.Receipt == "" {
				t.Fatal("missing durable receipt")
			}
			// A new service instance models process restart: receipt is in the database.
			restarted := merchantapp.New(f.orders, merchantstore.New(f.db), "test-shop")
			repeated, err := restarted.Deliver(req)
			if err != nil || repeated != first {
				t.Fatalf("replay did not return original receipt: %v", err)
			}
			if countDeliveries(t, f) != 1 {
				t.Fatal("duplicate delivery")
			}
			order, err := f.orders.GetByID(f.child.ID)
			if err != nil {
				t.Fatal(err)
			}
			if order.Status != constants.OrderStatusDelivered || order.Fulfillment == nil || order.Fulfillment.Payload == "" {
				t.Fatal("buyer-visible fulfillment not persisted")
			}
			if kind == "physical" && !strings.Contains(order.Fulfillment.Payload, "DEMO-TRACKING-001") {
				t.Fatal("tracking number missing")
			}
			parent, err := f.orders.GetByID(f.root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if parent.Status != constants.OrderStatusPartiallyDelivered {
				t.Fatal("parent status did not reflect partial delivery")
			}
			public, _ := json.Marshal(first)
			if strings.Contains(string(public), "CARD") || strings.Contains(string(public), "example.invalid") {
				t.Fatal("receipt leaked delivery content")
			}
		})
	}
}

func TestChangedKeyOrPayloadCannotDeliverOrderTwice(t *testing.T) {
	f := setup(t)
	req := f.request()
	if _, err := f.service.Deliver(req); err != nil {
		t.Fatal(err)
	}
	changed := req
	changed.IdempotencyKey = "fulfillment:different-job"
	if _, err := f.service.Deliver(changed); !errors.Is(err, merchantapp.ErrConflict) {
		t.Fatalf("different key: %v", err)
	}
	changed = req
	changed.Payload = merchantapp.Payload{Type: "card", Cards: []string{"OTHER-A", "OTHER-B"}}
	if _, err := f.service.Deliver(changed); !errors.Is(err, merchantapp.ErrConflict) {
		t.Fatalf("changed body: %v", err)
	}
	if countDeliveries(t, f) != 1 {
		t.Fatal("duplicate allocation")
	}
}

func TestCardLimitsMatchConnectorUnicodeAndQuantityContract(t *testing.T) {
	for _, quantity := range []int{2, 10000} {
		t.Run(fmt.Sprint(quantity), func(t *testing.T) {
			f := setup(t)
			if err := f.db.Model(&orderdomain.OrderItem{}).Where("id = ?", f.item.ID).Update("quantity", quantity).Error; err != nil {
				t.Fatal(err)
			}
			req := f.request()
			req.Quantity = quantity
			req.Payload.Cards = make([]string, quantity)
			for i := range req.Payload.Cards {
				req.Payload.Cards[i] = fmt.Sprintf("PUBLIC-%d", i)
			}
			if quantity == 2 {
				req.Payload.Cards[0] = strings.Repeat("示", 10000)
			}
			if _, err := f.service.Deliver(req); err != nil {
				t.Fatalf("valid connector payload rejected: %v", err)
			}
		})
	}
	f := setup(t)
	req := f.request()
	req.Payload.Cards[0] = strings.Repeat("示", 10001)
	if _, err := f.service.Deliver(req); !errors.Is(err, merchantapp.ErrInvalid) {
		t.Fatalf("oversized card should fail: %v", err)
	}
	req.Payload = merchantapp.Payload{Type: "physical", Carrier: strings.Repeat("示", 100), TrackingNumber: strings.Repeat("例", 100)}
	if _, err := f.service.Deliver(req); err != nil {
		t.Fatalf("valid Unicode tracking fields rejected: %v", err)
	}
}

func TestDeliveryRejectsIdentityAndPaymentMismatch(t *testing.T) {
	cases := map[string]func(*merchantapp.Delivery){"buyer": func(r *merchantapp.Delivery) { r.BuyerID = "user:8" }, "line": func(r *merchantapp.Delivery) { r.LineID = "9999" }, "product": func(r *merchantapp.Delivery) { r.ItemID = "99" }, "sku": func(r *merchantapp.Delivery) { r.SKUID = "99" }, "amount": func(r *merchantapp.Delivery) { r.Amount = "10.00" }, "currency": func(r *merchantapp.Delivery) { r.Currency = "USD" }, "quantity": func(r *merchantapp.Delivery) { r.Quantity = 1; r.Payload.Cards = r.Payload.Cards[:1] }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			req := f.request()
			change(&req)
			if _, err := f.service.Deliver(req); !errors.Is(err, merchantapp.ErrConflict) {
				t.Fatalf("expected conflict: %v", err)
			}
			if countDeliveries(t, f) != 0 {
				t.Fatal("rejected request wrote fulfillment")
			}
		})
	}
}

func TestUnpaidRefundedAndAutomaticOrdersCannotBeDelivered(t *testing.T) {
	cases := map[string]func(*fixture) error{
		"unpaid": func(f *fixture) error {
			return f.db.Model(&orderdomain.Order{}).Where("id = ?", f.child.ID).Update("paid_at", nil).Error
		},
		"underpaid": func(f *fixture) error {
			return f.db.Model(&orderdomain.Order{}).Where("id = ?", f.root.ID).Update("online_paid_amount", amount("1.00")).Error
		},
		"refunded parent": func(f *fixture) error {
			return f.db.Model(&orderdomain.Order{}).Where("id = ?", f.root.ID).Update("status", constants.OrderStatusPartiallyRefunded).Error
		},
		"refund record": func(f *fixture) error {
			return f.db.Create(&orderdomain.OrderRefundRecord{OrderID: f.root.ID, Amount: amount("1.00"), Currency: "CNY", Type: "manual"}).Error
		},
		"cancelled": func(f *fixture) error {
			return f.db.Model(&orderdomain.Order{}).Where("id = ?", f.child.ID).Update("status", constants.OrderStatusCanceled).Error
		},
		"auto stock": func(f *fixture) error {
			return f.db.Model(&orderdomain.OrderItem{}).Where("id = ?", f.item.ID).Update("fulfillment_type", constants.FulfillmentTypeAuto).Error
		},
		"multi line": func(f *fixture) error { row := f.item; row.ID = 0; return f.db.Create(&row).Error },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			if err := mutate(f); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.Deliver(f.request()); !errors.Is(err, merchantapp.ErrBlocked) {
				t.Fatalf("expected blocked: %v", err)
			}
			if countDeliveries(t, f) != 0 {
				t.Fatal("blocked order delivered")
			}
		})
	}
}

type beforeTransaction struct {
	ordercontract.Store
	before func()
}

func (s beforeTransaction) WithinTransaction(fn func(ordercontract.Transaction) error) error {
	s.before()
	return s.Store.WithinTransaction(fn)
}

func TestRefundAfterInitialReadIsRecheckedInsideTransaction(t *testing.T) {
	f := setup(t)
	if _, err := f.service.Get(f.child.OrderNo); err != nil {
		t.Fatal(err)
	}
	wrapped := beforeTransaction{Store: f.orders, before: func() {
		if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", f.root.ID).Update("status", constants.OrderStatusRefunded).Error; err != nil {
			t.Fatal(err)
		}
	}}
	svc := merchantapp.New(wrapped, merchantstore.New(f.db), "test-shop")
	if _, err := svc.Deliver(f.request()); !errors.Is(err, merchantapp.ErrBlocked) {
		t.Fatalf("refund raced through delivery: %v", err)
	}
	if countDeliveries(t, f) != 0 {
		t.Fatal("refunded order delivered")
	}
}

func TestConcurrentRetriesProduceOnePersistentReceipt(t *testing.T) {
	f := setup(t)
	req := f.request()
	var wg sync.WaitGroup
	results := make(chan merchantapp.Receipt, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); receipt, err := f.service.Deliver(req); results <- receipt; errs <- err }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for receipt := range results {
		if first == "" {
			first = receipt.Receipt
		}
		if receipt.Receipt != first {
			t.Fatal("receipt changed across retries")
		}
	}
	if countDeliveries(t, f) != 1 {
		t.Fatal("concurrent duplicate delivery")
	}
}

func TestPaginationAndTenantExclusion(t *testing.T) {
	f := setup(t)
	page, err := f.service.List(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Orders) != 1 || page.NextCursor == nil {
		t.Fatalf("pagination missing: %+v", page)
	}
	var after uint
	fmt.Sscan(*page.NextCursor, &after)
	second, err := f.service.List(after, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Orders) != 1 || second.Orders[0].OrderID == page.Orders[0].OrderID || second.NextCursor != nil {
		t.Fatal("pagination duplicated or omitted a leaf")
	}
	if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", f.root.ID).Update("reseller_id", 9).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Get(f.child.OrderNo); !errors.Is(err, merchantapp.ErrNotFound) {
		t.Fatalf("reseller order exposed: %v", err)
	}
}
