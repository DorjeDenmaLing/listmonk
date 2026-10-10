package manager

// denma: the campaign's preview text (denma_preview_text.go).

import (
	"strings"
	"testing"

	"github.com/knadh/listmonk/models"
)

func TestDenmaAddPreviewText(t *testing.T) {
	camp := func(typ, text string) *models.Campaign {
		return &models.Campaign{ContentType: typ, PreviewText: text}
	}
	withText := "  Join us <Saturday> & Sunday  "

	t.Run("after the body tag", func(t *testing.T) {
		in := `<html><head><style>body{}</style></head><BODY class="x" style="margin:0"><p>Hi</p></body></html>`
		out := string(denmaAddPreviewText(camp(models.CampaignContentTypeRichtext, withText), []byte(in)))

		start := `<html><head><style>body{}</style></head><BODY class="x" style="margin:0"><div style="display:none;`
		if !strings.HasPrefix(out, start) {
			t.Fatalf("not straight after <body>: %s", out)
		}
		if !strings.Contains(out, ">Join us &lt;Saturday&gt; &amp; Sunday&#847;") {
			t.Errorf("text not trimmed and escaped: %s", out)
		}
		if !strings.HasSuffix(out, `</div><p>Hi</p></body></html>`) {
			t.Errorf("rest of the body changed: %s", out)
		}
	})

	t.Run("first without a body tag", func(t *testing.T) {
		out := string(denmaAddPreviewText(camp(models.CampaignContentTypeHTML, withText), []byte(`<p>Hi</p>`)))
		if !strings.HasPrefix(out, `<div style="display:none;`) || !strings.HasSuffix(out, `</div><p>Hi</p>`) {
			t.Errorf("got %s", out)
		}
	})

	t.Run("unchanged", func(t *testing.T) {
		in := `<body><p>Hi</p></body>`
		for name, c := range map[string]*models.Campaign{
			"none":       camp(models.CampaignContentTypeRichtext, ""),
			"blank":      camp(models.CampaignContentTypeRichtext, "   "),
			"plain text": camp(models.CampaignContentTypePlain, withText),
		} {
			if out := string(denmaAddPreviewText(c, []byte(in))); out != in {
				t.Errorf("%s: got %s", name, out)
			}
		}
	})
}
