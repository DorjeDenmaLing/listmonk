package main

// denma: the sign-up webhooks' OpenAPI 3.1 description
// (denma_signup_openapi.json), served by each center at
// <center>/signup/openapi.json, with the center's own address in "servers".
// Its Config -> Webhooks page shows it as the webhooks' reference
// (views/denma-webhooks.html). TestDenmaSignupOpenAPI checks it against the
// fields the webhooks read.

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

//go:embed denma_signup_openapi.json
var denmaSignupOpenAPIDoc []byte

// denmaSignupFormFields are the fields a Form webhook reads (DenmaSignup).
var denmaSignupFormFields = []string{"email", "name", "first_name", "last_name", "consent",
	"form_id", "entry_id", "page_url", "attribs", "tags", "next", "nonce"}

// signupOpenAPI is the description with the center's address.
func (a *App) signupOpenAPI() ([]byte, error) {
	servers := []map[string]string{{
		"url":         strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/signup",
		"description": a.ko.String("app.site_name") + "'s sign-up webhooks.",
	}}
	return denmaJSONSet(denmaSignupOpenAPIDoc, "servers", servers)
}

// DenmaSignupOpenAPI serves the description.
func (a *App) DenmaSignupOpenAPI(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	b, err := a.signupOpenAPI()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Blob(http.StatusOK, echo.MIMEApplicationJSONCharsetUTF8, out.Bytes())
}
