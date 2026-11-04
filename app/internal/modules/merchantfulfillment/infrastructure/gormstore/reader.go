package gormstore

import (
	"errors"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	"gorm.io/gorm"
)

type Reader struct{ db *gorm.DB }

func New(db *gorm.DB) *Reader { return &Reader{db: db} }

func (r *Reader) FindIDByOrderNo(no string) (uint, error) {
	var row struct{ ID uint }
	err := r.db.Model(&orderdomain.Order{}).Select("id").Where("order_no = ? AND deleted_at IS NULL AND reseller_id IS NULL", no).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return row.ID, err
}

func (r *Reader) ListCandidates(after uint, limit int) ([]orderdomain.Order, error) {
	rows := []orderdomain.Order{}
	err := r.db.Model(&orderdomain.Order{}).Select("orders.id, orders.order_no").
		Where("orders.id > ? AND orders.deleted_at IS NULL AND orders.reseller_id IS NULL AND orders.paid_at IS NOT NULL AND orders.status IN ?", after, []string{"paid", "fulfilling"}).
		Where("NOT EXISTS (SELECT 1 FROM orders child WHERE child.parent_id = orders.id AND child.deleted_at IS NULL)").
		Where("(SELECT COUNT(*) FROM order_items i WHERE i.order_id = orders.id AND i.deleted_at IS NULL) = 1").
		Where("EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = orders.id AND i.deleted_at IS NULL AND i.fulfillment_type = ?)", "manual").
		Order("orders.id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}
