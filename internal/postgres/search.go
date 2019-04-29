// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"golang.org/x/discovery/internal"
	"golang.org/x/discovery/internal/derrors"
)

// InsertDocuments inserts a row for each package in the version.
//
// The returned error may be checked with derrors.IsInvalidArgument to
// determine if it was caused by an invalid version.
func (db *DB) InsertDocuments(ctx context.Context, version *internal.Version) error {
	if err := validateVersion(version); err != nil {
		return derrors.InvalidArgument(fmt.Sprintf("validateVersion(%+v): %v", version, err))
	}

	return db.Transact(func(tx *sql.Tx) error {
		return prepareAndExec(tx, `INSERT INTO documents (
				package_path,
				package_suffix,
				module_path,
				series_path,
				version,
				tsv_search_tokens
			) VALUES(
				 $1,
				 $2,
				 $3,
				 $4,
				 $5,
				SETWEIGHT(TO_TSVECTOR($6), 'A') ||
				SETWEIGHT(TO_TSVECTOR($7), 'A') ||
				SETWEIGHT(TO_TSVECTOR($8), 'B') ||
				SETWEIGHT(TO_TSVECTOR($9), 'C')
			) ON CONFLICT DO NOTHING;`, func(stmt *sql.Stmt) error {
			for _, p := range version.Packages {
				pathTokens := strings.Join([]string{p.Path, version.ModulePath, version.SeriesPath}, " ")
				if _, err := stmt.ExecContext(ctx, p.Path, p.Suffix, version.ModulePath, version.SeriesPath, version.Version, p.Name, pathTokens, p.Synopsis, version.ReadmeContents); err != nil {
					return fmt.Errorf("error inserting document for package %+v: %v", p, err)
				}
			}
			return nil
		})
	})
}

// SearchResult represents a single search result from SearchDocuments.
type SearchResult struct {
	// Rank is used to sort items in an array of SearchResult.
	Rank float64

	// NumImportedBy is the number of packages that import Package.
	NumImportedBy uint64

	// Package is the package data corresponding to this SearchResult.
	Package *internal.VersionedPackage

	// NumResults is the total number of packages that were returned for this search.
	NumResults uint64
}

// Search fetches packages from the database that match the terms
// provided, and returns them in order of relevance as a []*SearchResult.
func (db *DB) Search(ctx context.Context, terms []string, limit, offset int) ([]*SearchResult, error) {
	if limit == 0 {
		return nil, derrors.InvalidArgument(fmt.Sprintf("cannot search: limit cannot be 0"))
	}
	if len(terms) == 0 {
		return nil, derrors.InvalidArgument(fmt.Sprintf("cannot search: no terms"))
	}

	query := `
	WITH imported_by AS (
		SELECT to_path, COALESCE(COUNT(*),0) AS num_imported_by
		FROM (SELECT to_path, from_path FROM imports GROUP BY 1,2) i
		GROUP BY 1
	),
	docs AS (
		SELECT package_path, MAX(relevance) AS relevance
		FROM (
			SELECT package_path, version,
			ts_rank (tsv_search_tokens, to_tsquery($1)) AS relevance
			FROM documents
		) d
		WHERE relevance > POWER(10,-10)
		GROUP BY 1
	),
	latest_versions AS (
		SELECT DISTINCT ON (module_path) module_path, version, commit_time
		FROM versions
		ORDER BY module_path, major DESC, minor DESC, patch DESC, prerelease DESC
	)


	SELECT
		p.path AS package_path,
		v.version,
		p.module_path,
		p.name,
		p.synopsis,
		p.license_types,
		p.license_paths,
		v.commit_time,
		COALESCE(i.num_imported_by, 0) AS num_imported_by,
		d.relevance * log(exp(1) + COALESCE(i.num_imported_by, 0)) AS rank,
		COUNT(*) OVER() AS total
	FROM latest_versions v
	INNER JOIN vw_licensed_packages p
		ON p.module_path = v.module_path
		AND p.version=v.version
	INNER JOIN docs d
		ON d.package_path = p.path
	LEFT JOIN imported_by i
		ON i.to_path = p.path
	ORDER BY rank DESC
	LIMIT $2 OFFSET $3;`
	rows, err := db.QueryContext(ctx, query, strings.Join(terms, " | "), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("db.QueryContext(ctx, %s, %q, %d, %d): %v", query, terms, limit, offset, err)
	}
	defer rows.Close()

	var (
		path, version, modulePath, name, synopsis string
		licenseTypes, licensePaths                []string
		commitTime                                time.Time
		numImportedBy, total                      uint64
		rank                                      float64
		results                                   []*SearchResult
	)
	for rows.Next() {
		if err := rows.Scan(&path, &version, &modulePath, &name, &synopsis,
			pq.Array(&licenseTypes), pq.Array(&licensePaths), &commitTime, &numImportedBy, &rank, &total); err != nil {
			return nil, fmt.Errorf("rows.Scan(): %v", err)
		}

		lics, err := zipLicenseInfo(licenseTypes, licensePaths)
		if err != nil {
			return nil, fmt.Errorf("zipLicenseInfo(%v, %v): %v", licenseTypes, licensePaths, err)
		}
		results = append(results, &SearchResult{
			Rank:          rank,
			NumImportedBy: numImportedBy,
			NumResults:    total,
			Package: &internal.VersionedPackage{
				Package: internal.Package{
					Name:     name,
					Path:     path,
					Synopsis: synopsis,
					Licenses: lics,
				},
				VersionInfo: internal.VersionInfo{
					ModulePath: modulePath,
					Version:    version,
					CommitTime: commitTime,
				},
			},
		})
	}
	return results, nil
}
