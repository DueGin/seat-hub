package app

import (
	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/reservation"
)

func newReservationAdapter(deps featureDeps) api.ReservationService {
	api.Err(reservation.ErrInvalid, 422, "invalid_reservation", "请填写正确的邮箱，备注最多 200 字")
	api.Err(reservation.ErrInvalidNotice, 422, "invalid_reservation_notice", "请填写邮件标题和正文，标题最多 200 字且不能换行")
	api.Err(reservation.ErrInvalidFilter, 422, "invalid_filter", "筛选条件不正确")
	api.Err(reservation.ErrProduct, 422, "product_not_reservable", "该商品暂不接受预约")
	api.Err(reservation.ErrNotFound, 404, "reservation_not_found", "预约记录不存在")
	api.Err(reservation.ErrRateLimit, 429, "reservation_rate_limited", "预约操作过于频繁，请一小时后再试")
	api.Err(mail.ErrNotConfigured, 422, "mail_not_configured", "请先在设置中启用并配置邮件发送，再通知预约用户")
	return reservation.New(deps.DB, deps.Mail)
}
