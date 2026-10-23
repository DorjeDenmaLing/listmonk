package webhooks

import (
	"net/http/httptest"
	"testing"
)

// Azure's webhook with no secret set takes nothing (biofool/listmonk#5: it
// took anything); with one, only requests that carry it.
func TestDenmaAzureFailsClosed(t *testing.T) {
	req := httptest.NewRequest("POST", "/webhooks/service/azure", nil)
	if err := NewAzure("", "").verifyAuth(req); err == nil {
		t.Error("no secret set: a request was taken")
	}
	if err := NewAzure("s3cret", "").verifyAuth(req); err == nil {
		t.Error("a request without the secret was taken")
	}
	req.Header.Set("X-Listmonk-Webhook-Secret", "s3cret")
	if err := NewAzure("s3cret", "").verifyAuth(req); err != nil {
		t.Errorf("a request with the secret was refused: %v", err)
	}
}
