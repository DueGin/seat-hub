package ticket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const ticketColumns = `t.id,t.user_id,u.email,t.category,t.title,t.status,COALESCE(t.order_no,''),t.account_id,t.created_at,t.updated_at,t.last_message_at,t.last_message_role,t.user_read_message_id,t.admin_read_message_id,t.user_read_at,t.admin_read_at`

func unreadSQL(a Actor) string {
	role, marker := "admin", "user_read_message_id"
	if a.Admin {
		role, marker = "user", "admin_read_message_id"
	}
	return "EXISTS(SELECT 1 FROM ticket_messages m WHERE m.ticket_id=t.id AND m.role='" + role + "' AND m.id>t." + marker + ")"
}

type scanner interface{ Scan(...any) error }

func scanTicket(row scanner) (Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.UserID, &t.UserEmail, &t.Category, &t.Title, &t.Status, &t.OrderNo, &t.AccountID, &t.CreatedAt, &t.UpdatedAt, &t.LastMessageAt, &t.LastMessageRole, &t.UserReadMessageID, &t.AdminReadMessageID, &t.UserReadAt, &t.AdminReadAt, &t.Unread)
	return t, err
}
func lookup(ctx context.Context, q queryer, a Actor, id int64) (Ticket, error) {
	t, err := scanTicket(q.QueryRowContext(ctx, "SELECT "+ticketColumns+","+unreadSQL(a)+" FROM tickets t JOIN users u ON u.id=t.user_id WHERE t.id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("get ticket: %w", err)
	}
	if !a.Admin && t.UserID != a.ID {
		return Ticket{}, ErrForbidden
	}
	return t, nil
}
func (s *Service) Get(ctx context.Context, a Actor, id int64) (Detail, error) {
	return s.view(ctx, a, id, true)
}
func (s *Service) view(ctx context.Context, a Actor, id int64, mark bool) (Detail, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Detail{}, fmt.Errorf("begin conversation: %w", err)
	}
	defer tx.Rollback()
	t, err := lookup(ctx, tx, a, id)
	if err != nil {
		return Detail{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,ticket_id,author_id,role,body,created_at FROM ticket_messages WHERE ticket_id=? ORDER BY id", id)
	if err != nil {
		return Detail{}, fmt.Errorf("list messages: %w", err)
	}
	messages := []Message{}
	indices := map[int64]int{}
	var lastID int64
	for rows.Next() {
		m := Message{Attachments: []Attachment{}}
		if err := rows.Scan(&m.ID, &m.TicketID, &m.AuthorID, &m.Role, &m.Body, &m.CreatedAt); err != nil {
			rows.Close()
			return Detail{}, fmt.Errorf("scan message: %w", err)
		}
		indices[m.ID] = len(messages)
		messages = append(messages, m)
		lastID = m.ID
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Detail{}, fmt.Errorf("read messages: %w", err)
	}
	rows, err = tx.QueryContext(ctx, "SELECT id,name,content_type,size,message_id FROM ticket_attachments WHERE ticket_id=? ORDER BY id", id)
	if err != nil {
		return Detail{}, fmt.Errorf("list images: %w", err)
	}
	for rows.Next() {
		var att Attachment
		var mid int64
		if err := rows.Scan(&att.ID, &att.Name, &att.ContentType, &att.Size, &mid); err != nil {
			rows.Close()
			return Detail{}, fmt.Errorf("scan image: %w", err)
		}
		att.TicketID = &id
		messageID := mid
		att.MessageID = &messageID
		att.URL = attachmentURL(att.ID)
		if i, ok := indices[mid]; ok {
			messages[i].Attachments = append(messages[i].Attachments, att)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Detail{}, fmt.Errorf("read images: %w", err)
	}
	if mark {
		marker := "user"
		if a.Admin {
			marker = "admin"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tickets SET "+marker+"_read_message_id=MAX("+marker+"_read_message_id,?),"+marker+"_read_at=? WHERE id=?", lastID, now(), id); err != nil {
			return Detail{}, fmt.Errorf("mark ticket read: %w", err)
		}
		t, err = lookup(ctx, tx, a, id)
		if err != nil {
			return Detail{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Detail{}, fmt.Errorf("commit conversation: %w", err)
	}
	if !a.Admin {
		t.UserEmail = ""
	}
	return Detail{Ticket: t, Messages: messages}, nil
}
func (s *Service) List(ctx context.Context, a Actor, f Filter) (Page, error) {
	if f.Status != "" && !validStatus(f.Status) || f.Category != "" && !validCategory(f.Category) || f.UserID < 0 || len(f.Query) > 500 {
		return Page{}, ErrInvalid
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 20
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
	conditions := []string{"1=1"}
	args := []any{}
	if !a.Admin {
		conditions = append(conditions, "t.user_id=?")
		args = append(args, a.ID)
	} else if f.UserID > 0 {
		conditions = append(conditions, "t.user_id=?")
		args = append(args, f.UserID)
	}
	if f.Category != "" {
		conditions = append(conditions, "t.category=?")
		args = append(args, f.Category)
	}
	if f.Query != "" {
		conditions = append(conditions, "(instr(lower(t.title),lower(?))>0 OR instr(lower(u.email),lower(?))>0 OR EXISTS(SELECT 1 FROM ticket_messages m WHERE m.ticket_id=t.id AND instr(lower(m.body),lower(?))>0))")
		args = append(args, f.Query, f.Query, f.Query)
	}
	where := strings.Join(conditions, " AND ")
	p := Page{Items: []Ticket{}, Page: f.Page, PageSize: f.PageSize}
	if a.Admin {
		p.Counts = map[string]int64{"open": 0, "replied": 0, "closed": 0}
		rows, err := s.db.QueryContext(ctx, "SELECT t.status,COUNT(*) FROM tickets t JOIN users u ON u.id=t.user_id WHERE "+where+" GROUP BY t.status", args...)
		if err != nil {
			return p, fmt.Errorf("ticket counts: %w", err)
		}
		for rows.Next() {
			var status string
			var count int64
			if err := rows.Scan(&status, &count); err != nil {
				rows.Close()
				return p, fmt.Errorf("scan ticket count: %w", err)
			}
			p.Counts[status] = count
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return p, fmt.Errorf("read ticket counts: %w", err)
		}
	}
	if f.Status != "" {
		where += " AND t.status=?"
		args = append(args, f.Status)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tickets t JOIN users u ON u.id=t.user_id WHERE "+where, args...).Scan(&p.Total); err != nil {
		return p, fmt.Errorf("count tickets: %w", err)
	}
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.QueryContext(ctx, "SELECT "+ticketColumns+","+unreadSQL(a)+" FROM tickets t JOIN users u ON u.id=t.user_id WHERE "+where+" ORDER BY t.last_message_at DESC,t.id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return p, fmt.Errorf("list tickets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return p, fmt.Errorf("scan ticket: %w", err)
		}
		if !a.Admin {
			t.UserEmail = ""
		}
		p.Items = append(p.Items, t)
	}
	if err := rows.Err(); err != nil {
		return p, fmt.Errorf("read tickets: %w", err)
	}
	return p, nil
}
func (s *Service) Unread(ctx context.Context, a Actor) (int64, error) {
	where := "t.user_id=? AND " + unreadSQL(a)
	args := []any{a.ID}
	// Administrators need the actionable queue size, independent of who opened it.
	if a.Admin {
		where = "t.status='open'"
		args = nil
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tickets t WHERE "+where, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unread tickets: %w", err)
	}
	return count, nil
}
