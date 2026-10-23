package main

import (
	"net/http"

	"github.com/knadh/listmonk/internal/captcha"
	"github.com/labstack/echo/v4"
)

// denmaPublicCaptcha is the public form's CAPTCHA check for the public
// subscription API (/api/public/subscription), which listmonk didn't check
// (biofool/listmonk#5). The token is a form field, as the form's.
func (a *App) denmaPublicCaptcha(c echo.Context) error {
	if !a.captcha.IsEnabled() {
		return nil
	}
	var val string
	switch a.captcha.GetProvider() {
	case captcha.ProviderHCaptcha:
		val = c.FormValue("h-captcha-response")
	case captcha.ProviderAltcha:
		val = c.FormValue("altcha")
	}
	if val == "" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("public.invalidCaptcha"))
	}
	err, ok := a.captcha.Verify(val)
	if err != nil {
		a.log.Printf("captcha request failed: %v", err)
	}
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("public.invalidCaptcha"))
	}
	return nil
}
