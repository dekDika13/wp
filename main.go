package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

const (
	dsn          = "host=localhost user=dika password=12345678 dbname=event_lari port=5432 sslmode=disable"
	jwtSecretKey = "rahasia_event_lari_pak_haris_2026"
)

// ==========================================
// 1. SSE BROKER
// ==========================================
type Broker struct{ clients map[chan bool]bool; mu sync.Mutex }
func NewBroker() *Broker { return &Broker{clients: make(map[chan bool]bool)} }
func (b *Broker) AddClient() chan bool { b.mu.Lock(); defer b.mu.Unlock(); clientChan := make(chan bool); b.clients[clientChan] = true; return clientChan }
func (b *Broker) RemoveClient(clientChan chan bool) { b.mu.Lock(); defer b.mu.Unlock(); delete(b.clients, clientChan); close(clientChan) }
func (b *Broker) Broadcast() { b.mu.Lock(); defer b.mu.Unlock(); for clientChan := range b.clients { select { case clientChan <- true: default: } } }

// ==========================================
// 2. MODELS
// ==========================================
type Peserta struct {
	ID              string `json:"id"`
	NomorBIB        string `json:"nomor_bib"`
	NomorHP         string `json:"nomor_hp"`
	NamaLengkap     string `json:"nama_lengkap"`
	LokasiTreatment string `json:"lokasi_treatment"`
}

type ClaimRequest struct {
	Peserta
	EventID string `json:"event_id"`
}

type Voucher struct {
	ID          string    `json:"id"`
	PesertaID   string    `json:"peserta_id"`
	EventID     string    `json:"event_id"`
	KodeVoucher string    `json:"kode_voucher"`
	ClaimAt     time.Time `json:"claim_at"`
	ExpiredAt   time.Time `json:"expired_at"`
	IsRedeemed  bool      `json:"is_redeemed"`
}

type DashboardRow struct {
	VoucherID       string `json:"voucher_id"`
	EventNama       string `json:"event_nama"`
	NamaLengkap     string `json:"nama_lengkap"`
	NomorBIB        string `json:"nomor_bib"`
	NomorHP         string `json:"nomor_hp"`
	LokasiTreatment string `json:"lokasi_treatment"`
	KodeVoucher     string `json:"kode_voucher"`
	ClaimAt         string `json:"claim_at"`
	ExpiredAt       string `json:"expired_at"`
	IsRedeemed      bool   `json:"is_redeemed"`
}

type LokasiCabang struct {
	Nama string `json:"nama"`
	GMap string `json:"gmap"`
}

type Event struct {
	ID                string         `json:"id"`
	Nama              string         `json:"nama"`
	Judul             string         `json:"judul"`
	StartWaktuClaim   string         `json:"start_waktu_claim"` // TAMBAHAN WAKTU MULAI
	BatasWaktuClaim   string         `json:"batas_waktu_claim"`
	Background        string         `json:"background"`
	ToneColor         string         `json:"tone_color"`
	VoucherPrefix     string         `json:"voucher_prefix"`
	IsSequential      bool           `json:"is_sequential"`
	IsRequireBib      bool           `json:"is_require_bib"` 
	RequireValidation bool           `json:"require_validation"`
	ValNama           bool           `json:"val_nama"`
	ValBib            bool           `json:"val_bib"`
	ValHp             bool           `json:"val_hp"`
	CabangLokasi      []LokasiCabang `json:"cabang_lokasi"`
	IsActive          bool           `json:"is_active"`
}

type Whitelist struct {
	ID          string `json:"id"`
	EventID     string `json:"event_id"`
	NamaLengkap string `json:"nama_lengkap"`
	NomorBIB    string `json:"nomor_bib"`
	NomorHP     string `json:"nomor_hp"`
}

// ==========================================
// 3. REPOSITORY LAYER
// ==========================================
type Repository interface {
	CheckPesertaExists(eventID, bib, hp string) bool
	CreatePesertaAndVoucher(p Peserta, v Voucher) error
	GetVoucherByEventAndHp(eventID, bib, hp string) (Voucher, Peserta, error)
	GetAdminByUsername(username string) (string, string, error)
	GetVoucherByKodeOrBib(keyword string) (Voucher, Peserta, error)
	RedeemVoucher(voucherID string, adminID string) error
	DeletePeserta(bib string) error
	GetAllDashboardData() ([]DashboardRow, error)
	UpdateVoucherTime(voucherID string, newExpiredAt time.Time) error
	UpdateVoucherStatus(voucherID string, status bool) error
	GetAllEventsList() ([]Event, error)
	GetActiveEventsList() ([]Event, error)
	GetEventByID(id string) (Event, error)
	CreateEvent(e Event, lokasiJSON []byte) error
	UpdateEvent(e Event, lokasiJSON []byte) error
	DeleteEvent(eventID string) error
	SetEventStatus(eventID string, isActive bool) error
	IncrementAndGetSeq(eventID string) (int, error)
	
	CheckWhitelist(eventID, nama, bib, hp string, cNama, cBib, cHp bool) bool
	GetWhitelistByEvent(eventID string) ([]Whitelist, error)
	AddWhitelistBulk(eventID string, data []Whitelist) error
	DeleteWhitelistEntry(id string) error
	ClearWhitelist(eventID string) error
}

type repo struct{ db *sql.DB }

func (r *repo) CheckPesertaExists(eventID, bib, hp string) bool { 
	var exists bool
	r.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM peserta_lari p 
			JOIN vouchers v ON p.id = v.peserta_id 
			WHERE v.event_id = $1 AND (p.nomor_hp = $2 OR (p.nomor_bib = $3 AND p.nomor_bib != '' AND p.nomor_bib != '-'))
		)`, eventID, hp, bib).Scan(&exists)
	return exists 
}

func (r *repo) CreatePesertaAndVoucher(p Peserta, v Voucher) error { tx, err := r.db.Begin(); if err != nil { return err }; var pesertaID string; err = tx.QueryRow(`INSERT INTO peserta_lari (nomor_bib, nomor_hp, nama_lengkap, lokasi_treatment) VALUES ($1, $2, $3, $4) RETURNING id`, p.NomorBIB, p.NomorHP, p.NamaLengkap, p.LokasiTreatment).Scan(&pesertaID); if err != nil { tx.Rollback(); return err }; _, err = tx.Exec(`INSERT INTO vouchers (peserta_id, event_id, kode_voucher, claim_at, expired_at) VALUES ($1, $2, $3, $4, $5)`, pesertaID, v.EventID, v.KodeVoucher, v.ClaimAt, v.ExpiredAt); if err != nil { tx.Rollback(); return err }; return tx.Commit() }

func (r *repo) GetVoucherByEventAndHp(eventID, bib, hp string) (Voucher, Peserta, error) { 
	var v Voucher; var p Peserta
	query := `SELECT p.nama_lengkap, p.nomor_bib, p.lokasi_treatment, v.kode_voucher, v.claim_at, v.expired_at, v.is_redeemed FROM peserta_lari p JOIN vouchers v ON p.id = v.peserta_id WHERE v.event_id = $1 AND p.nomor_hp = $2`
	args := []interface{}{eventID, hp}
	if bib != "" && bib != "-" { query += ` AND p.nomor_bib = $3`; args = append(args, bib) }
	err := r.db.QueryRow(query, args...).Scan(&p.NamaLengkap, &p.NomorBIB, &p.LokasiTreatment, &v.KodeVoucher, &v.ClaimAt, &v.ExpiredAt, &v.IsRedeemed)
	return v, p, err 
}

func (r *repo) GetAdminByUsername(username string) (string, string, error) { var id, hash string; err := r.db.QueryRow(`SELECT id, password_hash FROM admins WHERE username=$1`, username).Scan(&id, &hash); return id, hash, err }
func (r *repo) GetVoucherByKodeOrBib(keyword string) (Voucher, Peserta, error) { var v Voucher; var p Peserta; err := r.db.QueryRow(`SELECT v.id, p.nama_lengkap, p.nomor_bib, p.lokasi_treatment, v.kode_voucher, v.is_redeemed, v.expired_at FROM peserta_lari p JOIN vouchers v ON p.id = v.peserta_id WHERE v.kode_voucher = $1 OR p.nomor_bib = $1`, keyword).Scan(&v.ID, &p.NamaLengkap, &p.NomorBIB, &p.LokasiTreatment, &v.KodeVoucher, &v.IsRedeemed, &v.ExpiredAt); return v, p, err }
func (r *repo) RedeemVoucher(voucherID string, adminID string) error { _, err := r.db.Exec(`UPDATE vouchers SET is_redeemed = true, redeemed_at = NOW(), redeemed_by = $1 WHERE id = $2 AND is_redeemed = false`, adminID, voucherID); return err }
func (r *repo) DeletePeserta(bib string) error { _, err := r.db.Exec(`DELETE FROM peserta_lari WHERE nomor_bib = $1`, bib); return err }
func (r *repo) GetAllDashboardData() ([]DashboardRow, error) { rows, err := r.db.Query(`SELECT v.id, p.nama_lengkap, p.nomor_bib, p.nomor_hp, p.lokasi_treatment, v.kode_voucher, v.claim_at, v.expired_at, v.is_redeemed, COALESCE(e.nama, 'Tanpa Event') FROM peserta_lari p JOIN vouchers v ON p.id = v.peserta_id LEFT JOIN events e ON v.event_id = e.id ORDER BY v.claim_at DESC`); if err != nil { return nil, err }; defer rows.Close(); var data []DashboardRow; for rows.Next() { var d DashboardRow; var cTime, eTime time.Time; if err := rows.Scan(&d.VoucherID, &d.NamaLengkap, &d.NomorBIB, &d.NomorHP, &d.LokasiTreatment, &d.KodeVoucher, &cTime, &eTime, &d.IsRedeemed, &d.EventNama); err == nil { d.ClaimAt = cTime.Format("2006-01-02T15:04:05"); d.ExpiredAt = eTime.Format("2006-01-02T15:04:05"); data = append(data, d) } }; return data, nil }
func (r *repo) UpdateVoucherTime(voucherID string, newExpiredAt time.Time) error { _, err := r.db.Exec(`UPDATE vouchers SET expired_at = $1 WHERE id = $2`, newExpiredAt, voucherID); return err }
func (r *repo) UpdateVoucherStatus(voucherID string, status bool) error { _, err := r.db.Exec(`UPDATE vouchers SET is_redeemed = $1 WHERE id = $2`, status, voucherID); return err }

func (r *repo) GetAllEventsList() ([]Event, error) {
	rows, err := r.db.Query(`SELECT id, nama, judul, start_waktu_claim, batas_waktu_claim, background, tone_color, voucher_prefix, is_sequential, is_require_bib, require_validation, val_nama, val_bib, val_hp, cabang_lokasi, is_active FROM events ORDER BY is_active DESC, created_at DESC`)
	if err != nil { return nil, err }
	defer rows.Close(); var events []Event
	for rows.Next() {
		var e Event; var lokasiJSON string; var tStart, tEnd time.Time
		if err := rows.Scan(&e.ID, &e.Nama, &e.Judul, &tStart, &tEnd, &e.Background, &e.ToneColor, &e.VoucherPrefix, &e.IsSequential, &e.IsRequireBib, &e.RequireValidation, &e.ValNama, &e.ValBib, &e.ValHp, &lokasiJSON, &e.IsActive); err == nil { e.StartWaktuClaim = tStart.Format("2006-01-02T15:04:05"); e.BatasWaktuClaim = tEnd.Format("2006-01-02T15:04:05"); var cabang []LokasiCabang; json.Unmarshal([]byte(lokasiJSON), &cabang); e.CabangLokasi = cabang; events = append(events, e) }
	}
	return events, nil
}

func (r *repo) GetActiveEventsList() ([]Event, error) {
	rows, err := r.db.Query(`SELECT id, nama, judul, start_waktu_claim, batas_waktu_claim, background, tone_color, voucher_prefix, is_sequential, is_require_bib, require_validation, val_nama, val_bib, val_hp, cabang_lokasi FROM events WHERE is_active = true ORDER BY created_at DESC`)
	if err != nil { return nil, err }
	defer rows.Close(); var events []Event
	for rows.Next() {
		var e Event; var lokasiJSON string; var tStart, tEnd time.Time
		if err := rows.Scan(&e.ID, &e.Nama, &e.Judul, &tStart, &tEnd, &e.Background, &e.ToneColor, &e.VoucherPrefix, &e.IsSequential, &e.IsRequireBib, &e.RequireValidation, &e.ValNama, &e.ValBib, &e.ValHp, &lokasiJSON); err == nil { e.StartWaktuClaim = tStart.Format("2006-01-02T15:04:05"); e.BatasWaktuClaim = tEnd.Format("2006-01-02T15:04:05"); var cabang []LokasiCabang; json.Unmarshal([]byte(lokasiJSON), &cabang); e.CabangLokasi = cabang; events = append(events, e) }
	}
	return events, nil
}

func (r *repo) GetEventByID(id string) (Event, error) {
	var e Event; var lokasiJSON string; var tStart, tEnd time.Time
	err := r.db.QueryRow(`SELECT id, nama, judul, start_waktu_claim, batas_waktu_claim, background, tone_color, voucher_prefix, is_sequential, is_require_bib, require_validation, val_nama, val_bib, val_hp, cabang_lokasi, is_active FROM events WHERE id = $1`, id).Scan(&e.ID, &e.Nama, &e.Judul, &tStart, &tEnd, &e.Background, &e.ToneColor, &e.VoucherPrefix, &e.IsSequential, &e.IsRequireBib, &e.RequireValidation, &e.ValNama, &e.ValBib, &e.ValHp, &lokasiJSON, &e.IsActive)
	e.StartWaktuClaim = tStart.Format("2006-01-02T15:04:05"); e.BatasWaktuClaim = tEnd.Format("2006-01-02T15:04:05"); var cabang []LokasiCabang; json.Unmarshal([]byte(lokasiJSON), &cabang); e.CabangLokasi = cabang; return e, err
}

func (r *repo) CreateEvent(e Event, lokasiJSON []byte) error { _, err := r.db.Exec(`INSERT INTO events (nama, judul, start_waktu_claim, batas_waktu_claim, background, tone_color, voucher_prefix, is_sequential, is_require_bib, require_validation, val_nama, val_bib, val_hp, cabang_lokasi, is_active) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, false)`, e.Nama, e.Judul, e.StartWaktuClaim, e.BatasWaktuClaim, e.Background, e.ToneColor, e.VoucherPrefix, e.IsSequential, e.IsRequireBib, e.RequireValidation, e.ValNama, e.ValBib, e.ValHp, string(lokasiJSON)); return err }
func (r *repo) UpdateEvent(e Event, lokasiJSON []byte) error { _, err := r.db.Exec(`UPDATE events SET nama=$1, judul=$2, start_waktu_claim=$3, batas_waktu_claim=$4, background=$5, tone_color=$6, voucher_prefix=$7, is_sequential=$8, is_require_bib=$9, require_validation=$10, val_nama=$11, val_bib=$12, val_hp=$13, cabang_lokasi=$14 WHERE id=$15`, e.Nama, e.Judul, e.StartWaktuClaim, e.BatasWaktuClaim, e.Background, e.ToneColor, e.VoucherPrefix, e.IsSequential, e.IsRequireBib, e.RequireValidation, e.ValNama, e.ValBib, e.ValHp, string(lokasiJSON), e.ID); return err }
func (r *repo) DeleteEvent(eventID string) error { _, err := r.db.Exec(`DELETE FROM events WHERE id = $1`, eventID); return err }
func (r *repo) SetEventStatus(eventID string, isActive bool) error { _, err := r.db.Exec(`UPDATE events SET is_active = $1 WHERE id = $2`, isActive, eventID); return err }
func (r *repo) IncrementAndGetSeq(eventID string) (int, error) { var seq int; err := r.db.QueryRow(`UPDATE events SET current_seq = current_seq + 1 WHERE id = $1 RETURNING current_seq`, eventID).Scan(&seq); return seq, err }

func (r *repo) CheckWhitelist(eventID, nama, bib, hp string, cNama, cBib, cHp bool) bool {
	query := `SELECT EXISTS(SELECT 1 FROM event_whitelist WHERE event_id=$1`
	var args []interface{}; args = append(args, eventID); argId := 2
	if cNama { query += fmt.Sprintf(" AND nama_lengkap ILIKE $%d", argId); args = append(args, "%"+strings.TrimSpace(nama)+"%"); argId++ }
	if cBib { query += fmt.Sprintf(" AND nomor_bib = $%d", argId); args = append(args, strings.TrimSpace(bib)); argId++ }
	if cHp { query += fmt.Sprintf(" AND nomor_hp = $%d", argId); args = append(args, strings.TrimSpace(hp)); argId++ }
	query += `)`
	var exists bool; r.db.QueryRow(query, args...).Scan(&exists); return exists
}

func (r *repo) GetWhitelistByEvent(eventID string) ([]Whitelist, error) { rows, err := r.db.Query(`SELECT id, event_id, nama_lengkap, nomor_bib, nomor_hp FROM event_whitelist WHERE event_id = $1 ORDER BY nama_lengkap ASC`, eventID); if err != nil { return nil, err }; defer rows.Close(); var list []Whitelist; for rows.Next() { var w Whitelist; if err := rows.Scan(&w.ID, &w.EventID, &w.NamaLengkap, &w.NomorBIB, &w.NomorHP); err == nil { list = append(list, w) } }; return list, nil }
func (r *repo) AddWhitelistBulk(eventID string, data []Whitelist) error { 
	tx, err := r.db.Begin(); if err != nil { return err }
	stmt, err := tx.Prepare(`INSERT INTO event_whitelist (event_id, nama_lengkap, nomor_bib, nomor_hp) VALUES ($1, $2, $3, $4) ON CONFLICT (event_id, nomor_bib) DO NOTHING`)
	if err != nil { tx.Rollback(); return err }; defer stmt.Close()
	for _, w := range data { if _, err := stmt.Exec(eventID, w.NamaLengkap, w.NomorBIB, w.NomorHP); err != nil { tx.Rollback(); return err } }
	return tx.Commit() 
}
func (r *repo) DeleteWhitelistEntry(id string) error { _, err := r.db.Exec(`DELETE FROM event_whitelist WHERE id = $1`, id); return err }
func (r *repo) ClearWhitelist(eventID string) error { _, err := r.db.Exec(`DELETE FROM event_whitelist WHERE event_id = $1`, eventID); return err }

// ==========================================
// 4. SERVICE LAYER & 5. HANDLERS
// ==========================================
type Service struct{ repo Repository }
type Handler struct { svc *Service; broker *Broker }

func (h *Handler) AdminSaveEvent(c *gin.Context) {
	var req Event; if err := c.ShouldBindJSON(&req); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "Data event tidak valid"}); return }
	if req.VoucherPrefix == "" { req.VoucherPrefix = "RUN-" }; lokasiJSON, _ := json.Marshal(req.CabangLokasi)
	if req.ID == "" { if err := h.svc.repo.CreateEvent(req, lokasiJSON); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal membuat event"}); return }; c.JSON(http.StatusOK, gin.H{"message": "Event berhasil dibuat (Status Inaktif)"})
	} else { if err := h.svc.repo.UpdateEvent(req, lokasiJSON); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal update event"}); return }; c.JSON(http.StatusOK, gin.H{"message": "Event berhasil diupdate!"}) }
}

func (h *Handler) AdminGetWhitelist(c *gin.Context) { list, err := h.svc.repo.GetWhitelistByEvent(c.Param("id")); if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengambil data"}); return }; c.JSON(http.StatusOK, list) }
func (h *Handler) AdminAddWhitelist(c *gin.Context) { var req []Whitelist; if err := c.ShouldBindJSON(&req); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "Data tidak valid"}); return }; if err := h.svc.repo.AddWhitelistBulk(c.Param("id"), req); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan data validasi"}); return }; c.JSON(http.StatusOK, gin.H{"message": "Peserta valid ditambahkan"}) }
func (h *Handler) AdminDeleteWhitelist(c *gin.Context) { if err := h.svc.repo.DeleteWhitelistEntry(c.Param("wid")); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal hapus data"}); return }; c.JSON(http.StatusOK, gin.H{"message": "Dihapus"}) }
func (h *Handler) AdminClearWhitelist(c *gin.Context) { if err := h.svc.repo.ClearWhitelist(c.Param("id")); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal clear data"}); return }; c.JSON(http.StatusOK, gin.H{"message": "Dihapus"}) }
func (h *Handler) AdminDeleteEvent(c *gin.Context) { if err := h.svc.repo.DeleteEvent(c.Param("id")); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menghapus event"}); return }; c.JSON(http.StatusOK, gin.H{"message": "Event berhasil dihapus"}) }
func (h *Handler) AdminSetEventStatus(c *gin.Context) { var req struct { IsActive bool `json:"is_active"` }; if err := c.ShouldBindJSON(&req); err != nil { return }; if err := h.svc.repo.SetEventStatus(c.Param("id"), req.IsActive); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal update status event"}); return }; c.JSON(http.StatusOK, gin.H{"message": "Status event diupdate!"}) }
func (h *Handler) AdminGetEventList(c *gin.Context) { events, err := h.svc.repo.GetAllEventsList(); if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengambil daftar event"}); return }; c.JSON(http.StatusOK, events) }
func (h *Handler) GetPublicEvents(c *gin.Context) { events, err := h.svc.repo.GetAllEventsList(); if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengambil event"}); return }; c.JSON(http.StatusOK, events) }
func (h *Handler) GetActiveEvents(c *gin.Context) { events, err := h.svc.repo.GetActiveEventsList(); if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengambil event"}); return }; c.JSON(http.StatusOK, events) }

func (h *Handler) ClaimVoucher(c *gin.Context) {
	var req ClaimRequest; lokasiWITA, _ := time.LoadLocation("Asia/Makassar"); if lokasiWITA == nil { lokasiWITA = time.FixedZone("WITA", 8*3600) }; waktuSekarang := time.Now().In(lokasiWITA)
	if err := c.ShouldBindJSON(&req); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "Data tidak valid"}); return }
	if req.EventID == "" { c.JSON(http.StatusBadRequest, gin.H{"error": "Event belum dipilih!"}); return }

	activeEvent, errEvent := h.svc.repo.GetEventByID(req.EventID)
	if errEvent != nil || !activeEvent.IsActive { c.JSON(http.StatusBadRequest, gin.H{"error": "Event tidak valid atau sudah ditutup!"}); return }

	// CEK WAKTU KLAIM
	startTime, _ := time.ParseInLocation("2006-01-02T15:04:05", activeEvent.StartWaktuClaim, lokasiWITA)
	if startTime.IsZero() { startTime, _ = time.ParseInLocation("2006-01-02T15:04", activeEvent.StartWaktuClaim, lokasiWITA) }
	expiredTime, _ := time.ParseInLocation("2006-01-02T15:04:05", activeEvent.BatasWaktuClaim, lokasiWITA)
	if expiredTime.IsZero() { expiredTime, _ = time.ParseInLocation("2006-01-02T15:04", activeEvent.BatasWaktuClaim, lokasiWITA) }

	if waktuSekarang.Before(startTime) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mohon maaf, sesi klaim voucher belum dibuka!"})
		return
	}
	if waktuSekarang.After(expiredTime) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mohon maaf, sesi klaim voucher sudah ditutup!"})
		return
	}

	if activeEvent.IsRequireBib && req.NomorBIB == "" { c.JSON(http.StatusBadRequest, gin.H{"error": "Nomor BIB Wajib diisi!"}); return }
	if !activeEvent.IsRequireBib && req.NomorBIB == "" { req.NomorBIB = "-" } 

	if h.svc.repo.CheckPesertaExists(activeEvent.ID, req.NomorBIB, req.NomorHP) { c.JSON(http.StatusConflict, gin.H{"error": "Anda sudah terdaftar dan mengklaim voucher di event ini!"}); return }

	if activeEvent.RequireValidation {
		if activeEvent.ValNama || activeEvent.ValBib || activeEvent.ValHp {
			if !h.svc.repo.CheckWhitelist(activeEvent.ID, req.NamaLengkap, req.NomorBIB, req.NomorHP, activeEvent.ValNama, activeEvent.ValBib, activeEvent.ValHp) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Pendaftaran Ditolak! Data Anda tidak sesuai dengan daftar peserta resmi dari panitia."})
				return
			}
		}
	}

	kode := ""
	if activeEvent.IsSequential { seq, _ := h.svc.repo.IncrementAndGetSeq(activeEvent.ID); kode = fmt.Sprintf("%s%04d", activeEvent.VoucherPrefix, seq)
	} else { rand.Seed(time.Now().UnixNano()); kode = fmt.Sprintf("%s%04d", activeEvent.VoucherPrefix, rand.Intn(9999)) }

	voucher := Voucher{ KodeVoucher: kode, ClaimAt: waktuSekarang, ExpiredAt: expiredTime, EventID: activeEvent.ID }
	if err := h.svc.repo.CreatePesertaAndVoucher(req.Peserta, voucher); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan data"}); return }
	h.broker.Broadcast(); c.JSON(http.StatusOK, gin.H{"message": "Voucher berhasil dibuat", "kode_voucher": voucher.KodeVoucher})
}

func (h *Handler) GetMyVoucher(c *gin.Context) { v, p, err := h.svc.repo.GetVoucherByEventAndHp(c.Query("event_id"), c.Query("bib"), c.Query("hp")); if err != nil { c.JSON(http.StatusNotFound, gin.H{"error": "Voucher tidak ditemukan di event ini"}); return }; c.JSON(http.StatusOK, gin.H{"nama": p.NamaLengkap, "bib": p.NomorBIB, "lokasi_treatment": p.LokasiTreatment, "kode_voucher": v.KodeVoucher, "claim_at": v.ClaimAt.Format("2006-01-02T15:04:05"), "expired_at": v.ExpiredAt.Format("2006-01-02T15:04:05"), "is_redeemed": v.IsRedeemed}) }
func (h *Handler) AdminLogin(c *gin.Context) { var req struct{ Username, Password string }; if err := c.ShouldBindJSON(&req); err != nil { return }; adminID, hash, err := h.svc.repo.GetAdminByUsername(req.Username); if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil { c.JSON(http.StatusUnauthorized, gin.H{"error": "Username/Password salah"}); return }; token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"admin_id": adminID, "exp": time.Now().Add(time.Hour * 8).Unix()}); tokenString, _ := token.SignedString([]byte(jwtSecretKey)); c.JSON(http.StatusOK, gin.H{"token": tokenString}) }
func (h *Handler) AdminCheckVoucher(c *gin.Context) { v, p, err := h.svc.repo.GetVoucherByKodeOrBib(c.Query("kode")); if err != nil { c.JSON(http.StatusNotFound, gin.H{"error": "Data tidak ditemukan"}); return }; c.JSON(http.StatusOK, gin.H{"voucher_id": v.ID, "nama": p.NamaLengkap, "bib": p.NomorBIB, "lokasi_treatment": p.LokasiTreatment, "kode_voucher": v.KodeVoucher, "is_redeemed": v.IsRedeemed, "expired_at": v.ExpiredAt.Format("2006-01-02T15:04:05")}) }
func (h *Handler) AdminRedeem(c *gin.Context) { var req struct { VoucherID string `json:"voucher_id"` }; if err := c.ShouldBindJSON(&req); err != nil { return }; if err := h.svc.repo.RedeemVoucher(req.VoucherID, c.MustGet("admin_id").(string)); err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": "Gagal menukar voucher"}); return }; h.broker.Broadcast(); c.JSON(http.StatusOK, gin.H{"message": "Voucher berhasil ditukar"}) }
func (h *Handler) AdminDelete(c *gin.Context) { if err := h.svc.repo.DeletePeserta(c.Param("bib")); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menghapus data"}); return }; h.broker.Broadcast(); c.JSON(http.StatusOK, gin.H{"message": "Data dihapus"}) }
func (h *Handler) AdminGetDashboard(c *gin.Context) { data, err := h.svc.repo.GetAllDashboardData(); if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengambil data"}); return }; c.JSON(http.StatusOK, data) }
func (h *Handler) AdminUpdateVoucherTime(c *gin.Context) { var req struct { VoucherID string `json:"voucher_id"`; NewTime string `json:"new_time"` }; if err := c.ShouldBindJSON(&req); err != nil { return }; parsedTime, _ := time.Parse(time.RFC3339, req.NewTime); if err := h.svc.repo.UpdateVoucherTime(req.VoucherID, parsedTime); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal update waktu"}); return }; h.broker.Broadcast(); c.JSON(http.StatusOK, gin.H{"message": "Waktu diupdate!"}) }
func (h *Handler) AdminUpdateVoucherStatus(c *gin.Context) { var req struct { VoucherID string `json:"voucher_id"`; Status bool `json:"status"` }; if err := c.ShouldBindJSON(&req); err != nil { return }; if err := h.svc.repo.UpdateVoucherStatus(req.VoucherID, req.Status); err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal update status"}); return }; h.broker.Broadcast(); c.JSON(http.StatusOK, gin.H{"message": "Status diupdate!"}) }
func (h *Handler) AdminLiveStream(c *gin.Context) { c.Writer.Header().Set("Content-Type", "text/event-stream"); c.Writer.Header().Set("Cache-Control", "no-cache"); c.Writer.Header().Set("Connection", "keep-alive"); clientChan := h.broker.AddClient(); defer h.broker.RemoveClient(clientChan); for { select { case <-clientChan: c.Writer.Write([]byte("data: refresh\n\n")); c.Writer.Flush(); case <-c.Request.Context().Done(): return } } }

// ==========================================
// 6. MIDDLEWARE & 7. AUTO MIGRATE
// ==========================================
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString := c.Query("token")
		if tokenString == "" { if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") { tokenString = strings.TrimPrefix(authHeader, "Bearer ") } }
		if tokenString == "" { c.JSON(http.StatusUnauthorized, gin.H{"error": "Token tidak ada"}); c.Abort(); return }
		token, _ := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) { return []byte(jwtSecretKey), nil })
		if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid { c.Set("admin_id", claims["admin_id"]); c.Next() } else { c.JSON(http.StatusUnauthorized, gin.H{"error": "Token expired"}); c.Abort() }
	}
}

func autoMigrate(db *sql.DB) {
	fmt.Println("⏳ Menyiapkan database...")
	query := `
    CREATE EXTENSION IF NOT EXISTS "pgcrypto";
    CREATE TABLE IF NOT EXISTS admins (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), username VARCHAR(50) UNIQUE NOT NULL, password_hash VARCHAR(255) NOT NULL, nama_petugas VARCHAR(100) NOT NULL, role VARCHAR(20) DEFAULT 'petugas', created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
    CREATE TABLE IF NOT EXISTS peserta_lari (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), nomor_bib VARCHAR(50) NOT NULL, nomor_hp VARCHAR(20) NOT NULL, nama_lengkap VARCHAR(100) NOT NULL, lokasi_treatment VARCHAR(50) NOT NULL, waktu_daftar TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
    CREATE TABLE IF NOT EXISTS events (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), nama VARCHAR(100) NOT NULL, judul VARCHAR(150) NOT NULL, batas_waktu_claim TIMESTAMP NOT NULL, background VARCHAR(255), tone_color VARCHAR(50), cabang_lokasi JSONB NOT NULL, is_active BOOLEAN DEFAULT false, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
    CREATE TABLE IF NOT EXISTS vouchers (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), peserta_id UUID NOT NULL REFERENCES peserta_lari(id) ON DELETE CASCADE, event_id UUID REFERENCES events(id) ON DELETE SET NULL, kode_voucher VARCHAR(50) UNIQUE NOT NULL, claim_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, expired_at TIMESTAMP NOT NULL, is_redeemed BOOLEAN DEFAULT FALSE, redeemed_at TIMESTAMP, redeemed_by UUID REFERENCES admins(id), UNIQUE(peserta_id));
    CREATE INDEX IF NOT EXISTS idx_peserta_hp ON peserta_lari(nomor_hp);
    CREATE INDEX IF NOT EXISTS idx_voucher_kode ON vouchers(kode_voucher);
	
	CREATE TABLE IF NOT EXISTS event_whitelist (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), 
		event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE, 
		nama_lengkap VARCHAR(150), 
		nomor_bib VARCHAR(50), 
		nomor_hp VARCHAR(30),
		UNIQUE (event_id, nomor_bib)
	);
	CREATE INDEX IF NOT EXISTS idx_ew_event ON event_whitelist(event_id);`
	db.Exec(query)
	
	db.Exec(`ALTER TABLE peserta_lari DROP CONSTRAINT IF EXISTS peserta_lari_nomor_bib_key`)
	db.Exec(`ALTER TABLE peserta_lari DROP CONSTRAINT IF EXISTS peserta_lari_nomor_hp_key`)

	// TAMBAHAN: Kolom start_waktu_claim (diberi default agar tabel lama tidak error)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS start_waktu_claim TIMESTAMP DEFAULT CURRENT_TIMESTAMP`)

	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS voucher_prefix VARCHAR(50) DEFAULT 'RUN-'`)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS is_sequential BOOLEAN DEFAULT false`)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS current_seq INT DEFAULT 0`)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS is_require_bib BOOLEAN DEFAULT true`)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS require_validation BOOLEAN DEFAULT false`)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS val_nama BOOLEAN DEFAULT false`)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS val_bib BOOLEAN DEFAULT false`)
	db.Exec(`ALTER TABLE events ADD COLUMN IF NOT EXISTS val_hp BOOLEAN DEFAULT false`)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM admins WHERE username = 'admin'").Scan(&count)
	if count == 0 { db.Exec(`INSERT INTO admins (username, password_hash, nama_petugas) VALUES ('admin', '$2a$12$hqsuBELc4h1eIuK1k.KNH.bWC6W1amNpsU292AuMPgTdUQjMtsNqS', 'Petugas Uji Coba')`) }
	fmt.Println("✅ Database Siap!")
}

// ==========================================
// 8. MAIN ROUTER
// ==========================================
func main() {
	db, err := sql.Open("postgres", dsn)
	if err != nil { log.Fatal(err) }
	defer db.Close()
	autoMigrate(db)

	repo := &repo{db: db}; svc := &Service{repo: repo}; broker := NewBroker(); h := &Handler{svc: svc, broker: broker}
	r := gin.Default()
	corsConfig := cors.DefaultConfig(); corsConfig.AllowOriginFunc = func(origin string) bool { return true }
	corsConfig.AllowCredentials = true; corsConfig.AllowHeaders = []string{"Origin", "Content-Length", "Content-Type", "Accept", "Authorization", "X-Requested-With"}
	corsConfig.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	r.Use(cors.New(corsConfig))

	api := r.Group("/api")
	{
		api.POST("/claim", h.ClaimVoucher)
		api.GET("/voucher", h.GetMyVoucher)
		api.GET("/events", h.GetPublicEvents)
		api.GET("/events/active", h.GetActiveEvents)
		r.GET("/api/peserta/stream", h.AdminLiveStream)
	}

	admin := r.Group("/api/admin")
	{
		admin.POST("/login", h.AdminLogin)
		protected := admin.Group("/"); protected.Use(AuthMiddleware())
		{
			protected.GET("/scan", h.AdminCheckVoucher)
			protected.POST("/redeem", h.AdminRedeem)
			protected.DELETE("/peserta/:bib", h.AdminDelete)
			protected.GET("/dashboard", h.AdminGetDashboard)
			protected.PUT("/voucher/time", h.AdminUpdateVoucherTime)
			protected.PUT("/voucher/status", h.AdminUpdateVoucherStatus)
			protected.GET("/stream", h.AdminLiveStream)

			protected.GET("/events", h.AdminGetEventList)
			protected.POST("/event", h.AdminSaveEvent) 
			protected.DELETE("/event/:id", h.AdminDeleteEvent)
			protected.PUT("/event/:id/status", h.AdminSetEventStatus) 

			protected.GET("/event/:id/whitelist", h.AdminGetWhitelist)
			protected.POST("/event/:id/whitelist", h.AdminAddWhitelist)
			protected.DELETE("/event/:id/whitelist", h.AdminClearWhitelist)
			protected.DELETE("/event/whitelist/:wid", h.AdminDeleteWhitelist)
		}
	}

	fmt.Println("🚀 Server Golang berjalan di Port 8080")
	r.Run("0.0.0.0:8080")
}
