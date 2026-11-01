package models

import "github.com/jmoiron/sqlx"

// denmaListsChunk is how many subscribers' lists are loaded at a time.
const denmaListsChunk = 1000

func (subs Subscribers) denmaLoadListsChunks(stmt *sqlx.Stmt) error {
	for i := 0; i < len(subs); i += denmaListsChunk {
		if err := subs[i:min(i+denmaListsChunk, len(subs))].LoadLists(stmt); err != nil {
			return err
		}
	}
	return nil
}
