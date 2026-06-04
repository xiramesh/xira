package channelcontrol

import (
	"context"
	"time"
)

const (
	PairingStatusWait      = "wait"
	PairingStatusScanned   = "scaned"
	PairingStatusConfirmed = "confirmed"
	PairingStatusExpired   = "expired"
	PairingStatusFailed    = "failed"
)

type PairingSnapshot struct {
	PairingID      string    `json:"pairing_id"`
	EntrypointID   string    `json:"entrypoint_id"`
	Status         string    `json:"status"`
	QRCode         string    `json:"qr_code,omitempty"`
	QRImageContent string    `json:"qr_image_content,omitempty"`
	AccountID      string    `json:"account_id,omitempty"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type AccountSnapshot struct {
	AccountID    string    `json:"account_id"`
	EntrypointID string    `json:"entrypoint_id"`
	UserID       string    `json:"user_id,omitempty"`
	BaseURL      string    `json:"base_url,omitempty"`
	StateDir     string    `json:"state_dir,omitempty"`
	Running      bool      `json:"running"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type PairingController interface {
	CreatePairing(context.Context) (PairingSnapshot, error)
	GetPairing(string) (PairingSnapshot, error)
	ListAccounts() ([]AccountSnapshot, error)
	DeleteAccount(context.Context, string) error
}
