package i18n

import "encoding/json"

// denma: LoadNew loads only the keys the language doesn't have yet.
func (i *I18n) LoadNew(b []byte) error {
	var l map[string]string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	for k, v := range l {
		if _, ok := i.langMap[k]; !ok {
			i.langMap[k] = v
		}
	}
	return nil
}
