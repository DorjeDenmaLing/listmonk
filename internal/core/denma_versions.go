package core

// denma: campaign and template updates inside a caller's transaction (cmd/denma_versions.go).

import (
	"net/http"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	"gopkg.in/volatiletech/null.v6"
)

// DenmaUpdateCampaignTx is UpdateCampaign's write, in tx.
func (c *Core) DenmaUpdateCampaignTx(tx *sqlx.Tx, id int, o models.Campaign, listIDs []int, mediaIDs []int) error {
	_, err := tx.Stmtx(c.q.UpdateCampaign).Exec(id,
		o.Name,
		o.Subject,
		o.FromEmail,
		o.Body,
		o.AltBody,
		o.ContentType,
		o.SendAt,
		o.Headers,
		o.Attribs,
		pq.StringArray(normalizeTags(o.Tags)),
		o.Messenger,
		o.TemplateID,
		pq.Array(listIDs),
		o.Archive,
		o.ArchiveSlug,
		o.ArchiveTemplateID,
		o.ArchiveMeta,
		pq.Array(mediaIDs),
		o.BodySource)
	if err != nil {
		c.log.Printf("error updating campaign: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.campaign}", "error", pqErrMsg(err)))
	}
	return nil
}

// DenmaUpdateTemplateTx is UpdateTemplate's write, in tx.
func (c *Core) DenmaUpdateTemplateTx(tx *sqlx.Tx, id int, name, subject string, body []byte, bodySource null.String) error {
	res, err := tx.Stmtx(c.q.UpdateTemplate).Exec(id, name, subject, body, bodySource)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.template}", "error", pqErrMsg(err)))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("globals.messages.notFound", "name", "{globals.terms.template}"))
	}
	return nil
}
