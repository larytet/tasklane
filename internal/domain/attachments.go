package domain

import (
	"context"
	"time"
)

type Attachment struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	MIME     string    `json:"mime"`
	Uploader string    `json:"uploader"`
	Created  time.Time `json:"created"`
}

func (s *Service) listAttachments(ctx context.Context, issueID string) ([]Attachment, error) {
	return []Attachment{}, nil
}
