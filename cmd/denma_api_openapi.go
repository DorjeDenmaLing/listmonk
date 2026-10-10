package main

// denma: the sending API's OpenAPI 3.1 description (denma_api_openapi.json),
// served by each center at <center>/v1/openapi.json, without a key, with the
// center's own address in "servers". Its Config -> API page shows it as the
// API's reference (views/denma-api.html). TestDenmaAPIOpenAPI checks it
// against the API's routes (denmaAPIRoutes) and what each needs of a key.

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

//go:embed denma_api_openapi.json
var denmaAPIOpenAPIDoc []byte

// apiOpenAPI is the description with the center's address, its keys in the
// file's order.
func (a *App) apiOpenAPI() ([]byte, error) {
	servers := []map[string]string{{
		"url":         strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/v1",
		"description": a.ko.String("app.site_name") + "'s API.",
	}}
	return denmaJSONSet(denmaAPIOpenAPIDoc, "servers", servers)
}

// DenmaAPIOpenAPI serves the description.
func (a *App) DenmaAPIOpenAPI(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	b, err := a.apiOpenAPI()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Blob(http.StatusOK, echo.MIMEApplicationJSONCharsetUTF8, out.Bytes())
}

// denmaJSONSet returns a JSON object with key's value replaced by val,
// keeping the order of its keys.
func denmaJSONSet(obj []byte, key string, val any) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i := 0; dec.More(); i++ {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if k == key {
			if raw, err = json.Marshal(val); err != nil {
				return nil, err
			}
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(raw)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
