package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/middleware"
)

type User struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

type Event struct {
	ID       int64  `json:"id,omitempty"`
	Title    string `json:"title,omitempty"`
	PublicFg bool   `json:"public,omitempty"`
	ClosedFg bool   `json:"closed,omitempty"`
	Price    int64  `json:"price,omitempty"`

	Total   int                `json:"total"`
	Remains int                `json:"remains"`
	Sheets  map[string]*Sheets `json:"sheets,omitempty"`
}

type Sheets struct {
	Total   int      `json:"total"`
	Remains int      `json:"remains"`
	Detail  []*Sheet `json:"detail,omitempty"`
	Price   int64    `json:"price"`
}

type Sheet struct {
	ID    int64  `json:"-"`
	Rank  string `json:"-"`
	Num   int64  `json:"num"`
	Price int64  `json:"-"`

	Mine           bool       `json:"mine,omitempty"`
	Reserved       bool       `json:"reserved,omitempty"`
	ReservedAt     *time.Time `json:"-"`
	ReservedAtUnix int64      `json:"reserved_at,omitempty"`
}

type Reservation struct {
	ID         int64      `json:"id"`
	EventID    int64      `json:"-"`
	SheetID    int64      `json:"-"`
	UserID     int64      `json:"-"`
	ReservedAt *time.Time `json:"-"`
	CanceledAt *time.Time `json:"-"`

	Event          *Event `json:"event,omitempty"`
	SheetRank      string `json:"sheet_rank,omitempty"`
	SheetNum       int64  `json:"sheet_num,omitempty"`
	Price          int64  `json:"price,omitempty"`
	ReservedAtUnix int64  `json:"reserved_at,omitempty"`
	CanceledAtUnix int64  `json:"canceled_at,omitempty"`
}

type Administrator struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

type EventSheetKey struct {
	EventId int64
	SheetId int64
}

type EventSheetReservation struct {
	UserID     int64
	ReservedAt time.Time
}

type EventSheetReservationCache struct {
	mu    []sync.RWMutex
	cache []map[int64]EventSheetReservation
}

func newEventSheetCache() EventSheetReservationCache {
	cache := make([]map[int64]EventSheetReservation, 1010)
	for i := 0; i < 1010; i++ {
		cache[i] = make(map[int64]EventSheetReservation)
	}
	return EventSheetReservationCache{
		mu:    make([]sync.RWMutex, 1010),
		cache: cache,
	}
}

func (c *EventSheetReservationCache) Get(eventId int64, sheetId int64) *EventSheetReservation {
	//key := EventSheetKey{eventId, sheetId}
	c.mu[sheetId].RLock()
	defer c.mu[sheetId].RUnlock()
	if v, ok := c.cache[sheetId][eventId]; ok {
		return &v
	}
	return nil
}

func (c *EventSheetReservationCache) Set(eventId int64, sheetId int64, reservation EventSheetReservation) {
	//key := EventSheetKey{eventId, sheetId}
	c.mu[sheetId].Lock()
	defer c.mu[sheetId].Unlock()
	c.cache[sheetId][eventId] = reservation
}

func (c *EventSheetReservationCache) Delete(eventId int64, sheetId int64) {
	//key := EventSheetKey{eventId, sheetId}
	c.mu[sheetId].Lock()
	defer c.mu[sheetId].Unlock()
	delete(c.cache[sheetId], eventId)
}

var eventSheetCache EventSheetReservationCache

func sessUserID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var userID int64
	if x, ok := sess.Values["user_id"]; ok {
		userID, _ = x.(int64)
	}
	return userID
}

func sessSetUserID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	sess.Values["user_id"] = id
	sess.Save(c.Request(), c.Response())
}

func sessDeleteUserID(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "user_id")
	sess.Save(c.Request(), c.Response())
}

func sessAdministratorID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var administratorID int64
	if x, ok := sess.Values["administrator_id"]; ok {
		administratorID, _ = x.(int64)
	}
	return administratorID
}

func sessSetAdministratorID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	sess.Values["administrator_id"] = id
	sess.Save(c.Request(), c.Response())
}

func sessDeleteAdministratorID(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "administrator_id")
	sess.Save(c.Request(), c.Response())
}

func loginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginUser(c); err != nil {
			return resError(c, "login_required", 401)
		}
		return next(c)
	}
}

func adminLoginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginAdministrator(c); err != nil {
			return resError(c, "admin_login_required", 401)
		}
		return next(c)
	}
}

func getLoginUser(c echo.Context) (*User, error) {
	userID := sessUserID(c)
	if userID == 0 {
		return nil, errors.New("not logged in")
	}
	var user User
	err := db.QueryRow("SELECT id, nickname FROM users WHERE id = ?", userID).Scan(&user.ID, &user.Nickname)
	return &user, err
}

func getLoginAdministrator(c echo.Context) (*Administrator, error) {
	administratorID := sessAdministratorID(c)
	if administratorID == 0 {
		return nil, errors.New("not logged in")
	}
	var administrator Administrator
	err := db.QueryRow("SELECT id, nickname FROM administrators WHERE id = ?", administratorID).Scan(&administrator.ID, &administrator.Nickname)
	if err != nil {
		log.Fatal("db.QueryRow:", err)
	}
	return &administrator, err
}

func getEvents(all bool) ([]*Event, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Commit()

	rows, err := tx.Query("SELECT * FROM events ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Title, &event.PublicFg, &event.ClosedFg, &event.Price); err != nil {
			return nil, err
		}
		if !all && !event.PublicFg {
			continue
		}
		events = append(events, &event)
	}
	for i, v := range events {
		event, err := getEvent(v.ID, -1)
		if err != nil {
			return nil, err
		}
		for k := range event.Sheets {
			event.Sheets[k].Detail = nil
		}
		events[i] = event
	}
	return events, nil
}

func getEvent(eventID, loginUserID int64) (*Event, error) {
	var event Event
	if err := db.QueryRow("SELECT * FROM events WHERE id = ?", eventID).Scan(&event.ID, &event.Title, &event.PublicFg, &event.ClosedFg, &event.Price); err != nil {
		return nil, err
	}
	event.Sheets = map[string]*Sheets{
		"S": &Sheets{},
		"A": &Sheets{},
		"B": &Sheets{},
		"C": &Sheets{},
	}

	for _, sheet := range allSheets {
		sheet := sheet
		var rankSheet *Sheets = event.Sheets[sheet.Rank]
		rankSheet.Price = event.Price + sheet.Price
		event.Total++
		rankSheet.Total++

		reservation := eventSheetCache.Get(event.ID, sheet.ID)
		if reservation != nil {
			sheet.Mine = reservation.UserID == loginUserID
			sheet.Reserved = true
			sheet.ReservedAtUnix = reservation.ReservedAt.Unix()
		} else {
			event.Remains++
			rankSheet.Remains++
		}

		rankSheet.Detail = append(rankSheet.Detail, &sheet)
	}

	return &event, nil
}

func sanitizeEvent(e *Event) *Event {
	sanitized := *e
	sanitized.Price = 0
	sanitized.PublicFg = false
	sanitized.ClosedFg = false
	return &sanitized
}

func fillinUser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if user, err := getLoginUser(c); err == nil {
			c.Set("user", user)
		}
		return next(c)
	}
}

func fillinAdministrator(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if administrator, err := getLoginAdministrator(c); err == nil {
			c.Set("administrator", administrator)
		} else {
			log.Printf("fillinAdministrator: %v", err)
		}
		return next(c)
	}
}

func validateRank(rank string) bool {
	var count int
	db.QueryRow("SELECT COUNT(*) FROM sheets WHERE `rank` = ?", rank).Scan(&count)
	return count > 0
}

type Renderer struct {
	templates *template.Template
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return r.templates.ExecuteTemplate(w, name, data)
}

func getIndexHandler(c echo.Context) error {
	events, err := getEvents(false)
	if err != nil {
		return err
	}
	for i, v := range events {
		events[i] = sanitizeEvent(v)
	}
	return c.Render(200, "index.tmpl", echo.Map{
		"events": events,
		"user":   c.Get("user"),
		"origin": c.Scheme() + "://" + c.Request().Host,
	})
}

var allSheets []Sheet

func mainInit() {
	allSheets = []Sheet{}
	rows, err := db.Query("SELECT * FROM sheets ORDER BY `rank`, num")
	if err != nil {
		log.Fatal("db.Query", err)
	}
	for rows.Next() {
		var sheet Sheet
		if err := rows.Scan(&sheet.ID, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
			log.Fatal("rows.Scane:", err)
		}
		allSheets = append(allSheets, sheet)
	}
	log.Print(len(allSheets))

	if err := rows.Close(); err != nil {
		log.Fatal(err)
	}

	eventSheetCache = newEventSheetCache()
	rows, err = db.Query("SELECT * FROM reservations WHERE canceled_at IS NULL")
	if err != nil {
		log.Fatal(err)
	}

	for rows.Next() {
		var reservation Reservation
		if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt); err != nil {
			log.Fatal(err)
		}

		if reservation.CanceledAt == nil {
			eventSheetCache.Set(reservation.EventID, reservation.SheetID, EventSheetReservation{reservation.UserID, *(reservation.ReservedAt)})
		}
	}

	if err := rows.Close(); err != nil {
		log.Fatal(err)
	}

}

func getInitializeHandler(c echo.Context) error {
	cmd := exec.Command("../../db/init.sh")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Print(err)
	}

	mainInit()

	f, err := os.Create("/tmp/cpuprofile")
	if err != nil {
		log.Fatal(err)
	}

	if err := pprof.StartCPUProfile(f); err != nil {
		log.Fatal("could not start CPU profile: ", err)
	}

	go func() {
		time.Sleep(time.Second * 70)
		defer pprof.StopCPUProfile()
	}()

	return c.NoContent(204)
}

func postUsersHandler(c echo.Context) error {
	var params struct {
		Nickname  string `json:"nickname"`
		LoginName string `json:"login_name"`
		Password  string `json:"password"`
	}
	c.Bind(&params)

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	var user User
	if err := tx.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.LoginName, &user.Nickname, &user.PassHash); err != sql.ErrNoRows {
		tx.Rollback()
		if err == nil {
			return resError(c, "duplicated", 409)
		}
		return err
	}

	res, err := tx.Exec("INSERT INTO users (login_name, pass_hash, nickname) VALUES (?, SHA2(?, 256), ?)", params.LoginName, params.Password, params.Nickname)
	if err != nil {
		tx.Rollback()
		return resError(c, "", 0)
	}
	userID, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return resError(c, "", 0)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return c.JSON(201, echo.Map{
		"id":       userID,
		"nickname": params.Nickname,
	})
}
func getUserHandler(c echo.Context) error {
	var user User
	if err := db.QueryRow("SELECT id, nickname FROM users WHERE id = ?", c.Param("id")).Scan(&user.ID, &user.Nickname); err != nil {
		return err
	}

	loginUser, err := getLoginUser(c)
	if err != nil {
		return err
	}
	if user.ID != loginUser.ID {
		return resError(c, "forbidden", 403)
	}

	rows, err := db.Query("SELECT r.*, s.rank AS sheet_rank, s.num AS sheet_num FROM reservations r INNER JOIN sheets s ON s.id = r.sheet_id WHERE r.user_id = ? ORDER BY IFNULL(r.canceled_at, r.reserved_at) DESC LIMIT 5", user.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var recentReservations []Reservation
	for rows.Next() {
		var reservation Reservation
		var sheet Sheet
		if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt, &sheet.Rank, &sheet.Num); err != nil {
			return err
		}

		event, err := getEvent(reservation.EventID, -1)
		if err != nil {
			return err
		}
		price := event.Sheets[sheet.Rank].Price
		event.Sheets = nil
		event.Total = 0
		event.Remains = 0

		reservation.Event = event
		reservation.SheetRank = sheet.Rank
		reservation.SheetNum = sheet.Num
		reservation.Price = price
		reservation.ReservedAtUnix = reservation.ReservedAt.Unix()
		if reservation.CanceledAt != nil {
			reservation.CanceledAtUnix = reservation.CanceledAt.Unix()
		}
		recentReservations = append(recentReservations, reservation)
	}
	if recentReservations == nil {
		recentReservations = make([]Reservation, 0)
	}

	var totalPrice int
	if err := db.QueryRow("SELECT IFNULL(SUM(e.price + s.price), 0) FROM reservations r INNER JOIN sheets s ON s.id = r.sheet_id INNER JOIN events e ON e.id = r.event_id WHERE r.user_id = ? AND r.canceled_at IS NULL", user.ID).Scan(&totalPrice); err != nil {
		return err
	}

	rows, err = db.Query("SELECT event_id FROM reservations WHERE user_id = ? GROUP BY event_id ORDER BY MAX(IFNULL(canceled_at, reserved_at)) DESC LIMIT 5", user.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var recentEvents []*Event
	for rows.Next() {
		var eventID int64
		if err := rows.Scan(&eventID); err != nil {
			return err
		}
		event, err := getEvent(eventID, -1)
		if err != nil {
			return err
		}
		for k := range event.Sheets {
			event.Sheets[k].Detail = nil
		}
		recentEvents = append(recentEvents, event)
	}
	if recentEvents == nil {
		recentEvents = make([]*Event, 0)
	}

	return c.JSON(200, echo.Map{
		"id":                  user.ID,
		"nickname":            user.Nickname,
		"recent_reservations": recentReservations,
		"total_price":         totalPrice,
		"recent_events":       recentEvents,
	})

}

func postLoginHandler(c echo.Context) error {
	var params struct {
		LoginName string `json:"login_name"`
		Password  string `json:"password"`
	}
	c.Bind(&params)

	user := new(User)
	if err := db.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.LoginName, &user.Nickname, &user.PassHash); err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "authentication_failed", 401)
		}
		return err
	}

	var passHash string
	if err := db.QueryRow("SELECT SHA2(?, 256)", params.Password).Scan(&passHash); err != nil {
		return err
	}
	if user.PassHash != passHash {
		return resError(c, "authentication_failed", 401)
	}

	sessSetUserID(c, user.ID)
	user, err := getLoginUser(c)
	if err != nil {
		return err
	}
	return c.JSON(200, user)
}

func postLogoutHandler(c echo.Context) error {
	sessDeleteUserID(c)
	return c.NoContent(204)
}

func getEventsHandler(c echo.Context) error {
	events, err := getEvents(true)
	if err != nil {
		return err
	}
	for i, v := range events {
		events[i] = sanitizeEvent(v)
	}
	return c.JSON(200, events)
}
func getEventHandler(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}

	loginUserID := int64(-1)
	if user, err := getLoginUser(c); err == nil {
		loginUserID = user.ID
	}

	event, err := getEvent(eventID, loginUserID)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "not_found", 404)
		}
		return err
	} else if !event.PublicFg {
		return resError(c, "not_found", 404)
	}
	return c.JSON(200, sanitizeEvent(event))
}
func postReserveHandler(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}
	var params struct {
		Rank string `json:"sheet_rank"`
	}
	c.Bind(&params)

	user, err := getLoginUser(c)
	if err != nil {
		log.Println("failed to get login user:", err)
		return err
	}

	event, err := getEvent(eventID, user.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "invalid_event", 404)
		}
		return err
	} else if !event.PublicFg {
		return resError(c, "invalid_event", 404)
	}

	if !validateRank(params.Rank) {
		return resError(c, "invalid_rank", 400)
	}

	var sheet Sheet
	var reservationID int64
	for {
		tx, err := db.Begin()
		if err := tx.QueryRow("SELECT * FROM sheets WHERE id NOT IN (SELECT sheet_id FROM reservations WHERE event_id = ? AND canceled_at IS NULL FOR UPDATE) AND `rank` = ? ORDER BY RAND() LIMIT 1", event.ID, params.Rank).Scan(&sheet.ID, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
			tx.Rollback()
			if err == sql.ErrNoRows {
				return resError(c, "sold_out", 409)
			}
			log.Println("re-try: rollback by", err)
			continue
		}

		t := time.Now()
		res, err := tx.Exec("INSERT INTO reservations (event_id, sheet_id, user_id, reserved_at) VALUES (?, ?, ?, ?)", event.ID, sheet.ID, user.ID, t.UTC().Format("2006-01-02 15:04:05.000000"))
		if err != nil {
			tx.Rollback()
			log.Println("re-try: rollback by", err)
			continue
		}
		reservationID, err = res.LastInsertId()
		if err != nil {
			tx.Rollback()
			log.Println("re-try: rollback by", err)
			continue
		}

		eventSheetCache.Set(event.ID, sheet.ID, EventSheetReservation{user.ID, t})
		if err := tx.Commit(); err != nil {
			tx.Rollback()
			log.Println("re-try: rollback by", err)
			continue
		}
		break
	}
	return c.JSON(202, echo.Map{
		"id":         reservationID,
		"sheet_rank": params.Rank,
		"sheet_num":  sheet.Num,
	})
}
func deleteReservationHandler(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}
	rank := c.Param("rank")
	num := c.Param("num")

	user, err := getLoginUser(c)
	if err != nil {
		return err
	}

	event, err := getEvent(eventID, user.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "invalid_event", 404)
		}
		return err
	} else if !event.PublicFg {
		return resError(c, "invalid_event", 404)
	}

	if !validateRank(rank) {
		return resError(c, "invalid_rank", 404)
	}

	var sheet Sheet
	if err := db.QueryRow("SELECT * FROM sheets WHERE `rank` = ? AND num = ?", rank, num).Scan(&sheet.ID, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "invalid_sheet", 404)
		}
		log.Println("we shouldn't reach here...", err)
		return err
	}

	for {
		tx, err := db.Begin()
		if err != nil {
			return err
		}

		var reservation Reservation
		if err := tx.QueryRow("SELECT * FROM reservations WHERE event_id = ? AND sheet_id = ? AND canceled_at IS NULL GROUP BY event_id HAVING reserved_at = MIN(reserved_at) FOR UPDATE", event.ID, sheet.ID).Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt); err != nil {
			tx.Rollback()
			if err == sql.ErrNoRows {
				return resError(c, "not_reserved", 400)
			}
			log.Println("re-try: rollback by", err)
			continue
		}

		if reservation.UserID != user.ID {
			// It's possible that the DB is overwritten after we read a researvation from the cache.
			reservation := eventSheetCache.Get(event.ID, sheet.ID)
			if reservation == nil || reservation.UserID != user.ID {
				return resError(c, "not_reserved", 400)
			}
			tx.Rollback()
			return resError(c, "not_permitted", 403)
		}

		if _, err := tx.Exec("UPDATE reservations SET canceled_at = ? WHERE id = ?", time.Now().UTC().Format("2006-01-02 15:04:05.000000"), reservation.ID); err != nil {
			tx.Rollback()
			log.Println("[update(delete)] re-try: rollback by", err)
			continue
		}

		eventSheetCache.Delete(reservation.EventID, reservation.SheetID)
		if err := tx.Commit(); err != nil {
			log.Println("[commit(delete)] re-try: rollback by", err)
			continue
		}
		break
	}

	return c.NoContent(204)
}
func getAdminHandler(c echo.Context) error {
	var events []*Event
	administrator := c.Get("administrator")
	log.Printf("getAdminHandler: %q", administrator)
	if administrator != nil {
		var err error
		if events, err = getEvents(true); err != nil {
			log.Printf("getEvents: %v", err)
			return err
		}
	}
	return c.Render(200, "admin.tmpl", echo.Map{
		"events":        events,
		"administrator": administrator,
		"origin":        c.Scheme() + "://" + c.Request().Host,
	})
}

func postAdminLoginHandler(c echo.Context) error {
	var params struct {
		LoginName string `json:"login_name"`
		Password  string `json:"password"`
	}
	c.Bind(&params)

	administrator := new(Administrator)
	if err := db.QueryRow("SELECT * FROM administrators WHERE login_name = ?", params.LoginName).Scan(&administrator.ID, &administrator.LoginName, &administrator.Nickname, &administrator.PassHash); err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "authentication_failed", 401)
		}
		return err
	}

	var passHash string
	if err := db.QueryRow("SELECT SHA2(?, 256)", params.Password).Scan(&passHash); err != nil {
		return err
	}
	if administrator.PassHash != passHash {
		return resError(c, "authentication_failed", 401)
	}

	sessSetAdministratorID(c, administrator.ID)
	administrator, err := getLoginAdministrator(c)
	if err != nil {
		return err
	}
	return c.JSON(200, administrator)
}

func postAdminLogoutHandler(c echo.Context) error {
	sessDeleteAdministratorID(c)
	return c.NoContent(204)
}

func getAdminEventsHandler(c echo.Context) error {
	events, err := getEvents(true)
	if err != nil {
		return err
	}
	return c.JSON(200, events)
}

func postAdminEventsHandler(c echo.Context) error {
	var params struct {
		Title  string `json:"title"`
		Public bool   `json:"public"`
		Price  int    `json:"price"`
	}
	c.Bind(&params)

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	res, err := tx.Exec("INSERT INTO events (title, public_fg, closed_fg, price) VALUES (?, ?, 0, ?)", params.Title, params.Public, params.Price)
	if err != nil {
		tx.Rollback()
		return err
	}
	eventID, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	event, err := getEvent(eventID, -1)
	if err != nil {
		return err
	}
	return c.JSON(200, event)
}

func getAdminEventHandler(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}
	event, err := getEvent(eventID, -1)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "not_found", 404)
		}
		return err
	}
	return c.JSON(200, event)
}

func postAdminEditEventHandler(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}

	var params struct {
		Public bool `json:"public"`
		Closed bool `json:"closed"`
	}
	c.Bind(&params)
	if params.Closed {
		params.Public = false
	}

	event, err := getEvent(eventID, -1)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "not_found", 404)
		}
		return err
	}

	if event.ClosedFg {
		return resError(c, "cannot_edit_closed_event", 400)
	} else if event.PublicFg && params.Closed {
		return resError(c, "cannot_close_public_event", 400)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE events SET public_fg = ?, closed_fg = ? WHERE id = ?", params.Public, params.Closed, event.ID); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	e, err := getEvent(eventID, -1)
	if err != nil {
		return err
	}
	c.JSON(200, e)
	return nil

}

func getAdminReportsEventHandler(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}

	event, err := getEvent(eventID, -1)
	if err != nil {
		return err
	}

	rows, err := db.Query("SELECT r.*, s.rank AS sheet_rank, s.num AS sheet_num, s.price AS sheet_price, e.price AS event_price FROM reservations r INNER JOIN sheets s ON s.id = r.sheet_id INNER JOIN events e ON e.id = r.event_id WHERE r.event_id = ? ORDER BY reserved_at ASC FOR UPDATE", event.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var reports []Report
	for rows.Next() {
		var reservation Reservation
		var sheet Sheet
		if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt, &sheet.Rank, &sheet.Num, &sheet.Price, &event.Price); err != nil {
			return err
		}
		report := Report{
			ReservationID: reservation.ID,
			EventID:       event.ID,
			Rank:          sheet.Rank,
			Num:           sheet.Num,
			UserID:        reservation.UserID,
			SoldAt:        reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
			Price:         event.Price + sheet.Price,
		}
		if reservation.CanceledAt != nil {
			report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
		}
		reports = append(reports, report)
	}
	return renderReportCSV(c, reports)
}

func getAdminReportsHandler(c echo.Context) error {
	rows, err := db.Query("select r.*, s.rank as sheet_rank, s.num as sheet_num, s.price as sheet_price, e.id as event_id, e.price as event_price from reservations r inner join sheets s on s.id = r.sheet_id inner join events e on e.id = r.event_id order by reserved_at asc for update")
	if err != nil {
		log.Print("query (/admin/api/reports/): ", err)
		return err
	}
	defer rows.Close()

	var reports []Report
	for rows.Next() {
		var reservation Reservation
		var sheet Sheet
		var event Event
		if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt, &sheet.Rank, &sheet.Num, &sheet.Price, &event.ID, &event.Price); err != nil {
			log.Print("scan (/admin/api/reports/): ", err)
			return err
		}
		report := Report{
			ReservationID: reservation.ID,
			EventID:       event.ID,
			Rank:          sheet.Rank,
			Num:           sheet.Num,
			UserID:        reservation.UserID,
			SoldAt:        reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
			Price:         event.Price + sheet.Price,
		}
		if reservation.CanceledAt != nil {
			report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
		}
		reports = append(reports, report)
	}
	return renderReportCSV(c, reports)
}

var db *sql.DB

func main() {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4",
		os.Getenv("DB_USER"), os.Getenv("DB_PASS"),
		os.Getenv("DB_HOST"), os.Getenv("DB_PORT"),
		os.Getenv("DB_DATABASE"),
	)

	var err error
	logfile, err := os.Create("/tmp/log.log")
	if err != nil {
		panic("cannnot open /tmp/log.log:" + err.Error())
	}
	defer logfile.Close()
	log.SetOutput(io.MultiWriter(logfile, os.Stdout))
	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)

	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}

	mainInit()

	e := echo.New()
	funcs := template.FuncMap{
		"encode_json": func(v interface{}) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
	}
	e.Renderer = &Renderer{
		templates: template.Must(template.New("").Delims("[[", "]]").Funcs(funcs).ParseGlob("views/*.tmpl")),
	}
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("secret"))))

	echolog, err := os.Create("/tmp/echo.log")
	if err != nil {
		panic("cannnot open /tmp/echo.log:" + err.Error())
	}

	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{Output: echolog}))

	e.Static("/", "public")
	e.GET("/", getIndexHandler, fillinUser)
	e.GET("/initialize", getInitializeHandler)
	e.POST("/api/users", postUsersHandler)
	e.GET("/api/users/:id", getUserHandler, loginRequired)
	e.POST("/api/actions/login", postLoginHandler)
	e.POST("/api/actions/logout", postLogoutHandler, loginRequired)
	e.GET("/api/events", getEventsHandler)
	e.GET("/api/events/:id", getEventHandler)
	e.POST("/api/events/:id/actions/reserve", postReserveHandler, loginRequired)
	e.DELETE("/api/events/:id/sheets/:rank/:num/reservation", deleteReservationHandler, loginRequired)
	e.GET("/admin/", getAdminHandler, fillinAdministrator)
	e.POST("/admin/api/actions/login", postAdminLoginHandler)
	e.POST("/admin/api/actions/logout", postAdminLogoutHandler, adminLoginRequired)
	e.GET("/admin/api/events", getAdminEventsHandler, adminLoginRequired)
	e.POST("/admin/api/events", postAdminEventsHandler, adminLoginRequired)
	e.GET("/admin/api/events/:id", getAdminEventHandler, adminLoginRequired)
	e.POST("/admin/api/events/:id/actions/edit", postAdminEditEventHandler, adminLoginRequired)
	e.GET("/admin/api/reports/events/:id/sales", getAdminReportsEventHandler, adminLoginRequired)
	e.GET("/admin/api/reports/sales", getAdminReportsHandler, adminLoginRequired)

	e.Start(":8080")
}

type Report struct {
	ReservationID int64
	EventID       int64
	Rank          string
	Num           int64
	UserID        int64
	SoldAt        string
	CanceledAt    string
	Price         int64
}

func renderReportCSV(c echo.Context, reports []Report) error {
	sort.Slice(reports, func(i, j int) bool { return strings.Compare(reports[i].SoldAt, reports[j].SoldAt) < 0 })

	body := bytes.NewBufferString("reservation_id,event_id,rank,num,price,user_id,sold_at,canceled_at\n")
	for _, v := range reports {
		body.WriteString(fmt.Sprintf("%d,%d,%s,%d,%d,%d,%s,%s\n",
			v.ReservationID, v.EventID, v.Rank, v.Num, v.Price, v.UserID, v.SoldAt, v.CanceledAt))
	}

	c.Response().Header().Set("Content-Type", `text/csv; charset=UTF-8`)
	c.Response().Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
	_, err := io.Copy(c.Response(), body)
	if err != nil {
		log.Print("render report csv:", err)
	}
	return err
}

func resError(c echo.Context, e string, status int) error {
	if e == "" {
		e = "unknown"
	}
	if status < 100 {
		status = 500
	}
	return c.JSON(status, map[string]string{"error": e})
}
