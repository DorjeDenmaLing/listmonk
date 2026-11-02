package main

import (
	"errors"
	"fmt"

	"github.com/knadh/listmonk/models"
)

const (
	denmaAttachDefaultMB = 10 // a center's limit until it sets its own (denma.attachment_max_mb)
	denmaAttachCeilingMB = 25 // the most a center can set
)

// denmaAttachMB is the most a campaign's attachments can come to in this center, in MB; 0 is no attachments.
func (a *App) denmaAttachMB() int {
	if !a.ko.Exists("denma.attachment_max_mb") {
		return denmaAttachDefaultMB
	}
	return a.ko.Int("denma.attachment_max_mb")
}

// denmaAttachRefusal says why these attachments are refused, or "" if they aren't.
func denmaAttachRefusal(count int, size int64, maxMB int) string {
	if count == 0 {
		return ""
	}
	if maxMB <= 0 {
		return "This center doesn't send attachments (Config -> General). Remove them, or put the files in Media and link to them."
	}
	if size <= int64(maxMB)<<20 {
		return ""
	}
	return fmt.Sprintf("These attachments come to %.1f MB, and this center's e-mails can carry %d MB (Config -> General). Put large files in Media and link to them instead.",
		float64(size)/(1<<20), maxMB)
}

// denmaCheckAttachments refuses Media files over the center's limit.
func (a *App) denmaCheckAttachments(ids []int) error {
	maxMB := a.denmaAttachMB()
	var size int64
	count := 0
	seen := make(map[int]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		count++
		if maxMB <= 0 {
			continue
		}
		m, err := a.core.GetMedia(id, "", "", a.media)
		if err != nil {
			continue
		}
		b, err := a.media.GetBlob(m.URL)
		if err != nil {
			continue
		}
		size += int64(len(b))
	}
	if why := denmaAttachRefusal(count, size, maxMB); why != "" {
		return denmaBadRequest(errors.New(why))
	}
	return nil
}

// denmaCheckCampaignAttachments checks a saved campaign's attachments before it starts; one that's paused can go on.
func (a *App) denmaCheckCampaignAttachments(id int) error {
	camp, err := a.core.GetCampaign(id, "", "")
	if err != nil {
		return err
	}
	if camp.Status == models.CampaignStatusPaused {
		return nil
	}
	var ids []int
	for _, m := range camp.ParsedMedia() {
		ids = append(ids, m.ID)
	}
	return a.denmaCheckAttachments(ids)
}

// denmaAttachTab reports whether a campaign's page has the Attachments tab: not at 0, unless it has some to remove.
func (a *App) denmaAttachTab(camp *models.Campaign) bool {
	return a.denmaAttachMB() > 0 || (camp != nil && len(camp.ParsedMedia()) > 0)
}
