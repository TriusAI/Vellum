package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// DupDoc is one document participating in a content-duplicate group.
type DupDoc struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
}

// DupGroup is a set of documents whose bytes are identical (their SHA-256 and
// SHA3-256 both match). Canonical is the entry kept; Duplicates are the
// redundant copies to fold in.
type DupGroup struct {
	Canonical  DupDoc   `json:"canonical"`
	Duplicates []DupDoc `json:"duplicates"`
}

// FindDuplicateGroups reports groups of documents that share identical
// content. It is deliberately cheap for the common case: documents are
// bucketed by the SHA-256 already stored at ingest, and only buckets with
// more than one member are re-hashed (with SHA3-256) to confirm. A bucket
// whose only surviving member is a single file is skipped entirely.
//
// Documents whose file has gone missing are ignored (their bytes cannot be
// verified); a member that no longer matches is simply not grouped.
func FindDuplicateGroups(conn *sql.DB) ([]DupGroup, error) {
	rows, err := conn.Query(
		"SELECT id, path, sha256 FROM documents " +
			"WHERE sha256 IS NOT NULL AND sha256<>'' ORDER BY id")
	if err != nil {
		return nil, err
	}
	type doc struct {
		id     int64
		path   string
		sha256 string
	}
	bySum := map[string][]doc{}
	var order []string
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.id, &d.path, &d.sha256); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := bySum[d.sha256]; !ok {
			order = append(order, d.sha256)
		}
		bySum[d.sha256] = append(bySum[d.sha256], d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var groups []DupGroup
	for _, key := range order {
		cand := bySum[key]
		if len(cand) < 2 {
			continue // a unique file cannot be a duplicate
		}
		// Re-hash the candidates (both hashes) and regroup by the fresh
		// pair: this confirms the match and survives a file changed since
		// ingest.
		byPair := map[string][]DupDoc{}
		var pairOrder []string
		for _, d := range cand {
			if _, err := os.Stat(d.path); err != nil {
				continue
			}
			h256, h3, err := contentHashes(d.path)
			if err != nil {
				continue
			}
			conn.Exec("UPDATE documents SET sha256=?, sha3=? WHERE id=?", h256, h3, d.id)
			pair := h256 + ":" + h3
			if _, ok := byPair[pair]; !ok {
				pairOrder = append(pairOrder, pair)
			}
			byPair[pair] = append(byPair[pair], DupDoc{ID: d.id, Path: d.path})
		}
		for _, pair := range pairOrder {
			g := byPair[pair]
			if len(g) < 2 {
				continue
			}
			groups = append(groups, DupGroup{Canonical: g[0], Duplicates: g[1:]})
		}
	}
	return groups, nil
}

// MergeDocument folds the duplicate document dup into canonical: its tags and
// collection memberships move across (the canonical wins conflicts), chats
// scoped to the duplicate are repointed at the canonical, and the duplicate
// row is deleted (its chunks cascade). The duplicate's cached cover is
// removed. Neither source file is touched on disk.
func MergeDocument(conn *sql.DB, baseDir string, canonical, dup int64) error {
	for _, q := range []string{
		"INSERT OR IGNORE INTO doc_tags(doc_id, tag, source) " +
			"SELECT ?, tag, source FROM doc_tags WHERE doc_id=?",
		"INSERT OR IGNORE INTO collection_docs(collection_id, doc_id, added_at) " +
			"SELECT collection_id, ?, added_at FROM collection_docs WHERE doc_id=?",
	} {
		if _, err := conn.Exec(q, canonical, dup); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(
		"UPDATE chat_sessions SET scope_value=? WHERE scope_kind='document' AND scope_value=?",
		fmt.Sprintf("%d", canonical), fmt.Sprintf("%d", dup)); err != nil {
		return err
	}
	if _, err := conn.Exec("DELETE FROM documents WHERE id=?", dup); err != nil {
		return err
	}
	os.Remove(filepath.Join(baseDir, "covers", fmt.Sprintf("cover-%d.png", dup)))
	return nil
}
