package core

// denma: a subscriber query's plan may read only the current schema's tables.

import (
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// denmaCheckPlanSchemas fails if the EXPLAIN JSON plan reads a relation of any
// schema but the transaction's current one (the center's).
func denmaCheckPlanSchemas(tx *sqlx.Tx, plan string) error {
	var cur string
	if err := tx.Get(&cur, "SELECT current_schema()"); err != nil {
		return err
	}
	return denmaPlanInSchema(plan, cur)
}

// denmaPlanInSchema fails if a relation in the plan isn't in schema.
func denmaPlanInSchema(plan, schema string) error {
	var plans []map[string]any
	if err := json.Unmarshal([]byte(plan), &plans); err != nil {
		return err
	}
	for _, p := range plans {
		if err := denmaNodeInSchema(p, schema); err != nil {
			return err
		}
	}
	return nil
}

func denmaNodeInSchema(node map[string]any, schema string) error {
	if rel, ok := node["Relation Name"].(string); ok {
		if s, _ := node["Schema"].(string); s != schema {
			return fmt.Errorf("table '%s' is not allowed", rel)
		}
	}
	for _, v := range node {
		switch v := v.(type) {
		case map[string]any:
			if err := denmaNodeInSchema(v, schema); err != nil {
				return err
			}
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					if err := denmaNodeInSchema(m, schema); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
