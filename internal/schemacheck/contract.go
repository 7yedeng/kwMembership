// Package schemacheck inspects schema metadata without opening the worker
// store, reading business rows, or creating/migrating a database.
package schemacheck

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"regexp"
)

// contract.json is an explicit, reviewed declaration. It is not generated from
// an input database, executed as DDL, or inferred from whatever schema exists.
//
//go:embed contract.json
var contractJSON []byte

type contract struct {
	ID                   string          `json:"id"`
	Version              int             `json:"version"`
	MatrixBaseline       string          `json:"matrixBaseline"`
	ReviewedSourceCommit string          `json:"reviewedSourceCommit"`
	Scope                string          `json:"scope"`
	Tables               []tableContract `json:"tables"`
	Unknown              []string        `json:"unknown"`
}

type tableContract struct {
	Name         string           `json:"name"`
	RequiredWhen string           `json:"requiredWhen"`
	Sources      []string         `json:"sources"`
	Columns      []columnContract `json:"columns"`
	Unique       []uniqueContract `json:"unique"`
	PrimaryKey   []string         `json:"primaryKey,omitempty"`
	Unknown      []string         `json:"unknown"`
}

type columnContract struct {
	Name            string   `json:"name"`
	RequiredWhen    string   `json:"requiredWhen"`
	TypeHint        string   `json:"typeHint"`
	TypeEvidence    string   `json:"typeEvidence"`
	WriteSources    []string `json:"writeSources,omitempty"`
	Affinity        string   `json:"affinity,omitempty"`
	RequireNotNull  bool     `json:"requireNotNull,omitempty"`
	RequireNullable bool     `json:"requireNullable,omitempty"`
	DefaultLiteral  *string  `json:"defaultLiteral,omitempty"`
}

type uniqueContract struct {
	Columns      []string `json:"columns"`
	RequiredWhen string   `json:"requiredWhen"`
	Source       string   `json:"source"`
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var diagnosticCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func loadContract() (contract, error) {
	var c contract
	decoder := json.NewDecoder(bytes.NewReader(contractJSON))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || c.ID == "" || c.Version < 1 {
		return contract{}, fail("SCHEMA_CONTRACT_INVALID")
	}
	tables := map[string]bool{}
	for _, table := range c.Tables {
		if !identifier.MatchString(table.Name) || tables[table.Name] || !validCondition(table.RequiredWhen) {
			return contract{}, fail("SCHEMA_CONTRACT_INVALID")
		}
		tables[table.Name] = true
		columns := map[string]bool{}
		for _, column := range table.Columns {
			if !identifier.MatchString(column.Name) || columns[column.Name] || !validCondition(column.RequiredWhen) ||
				column.RequireNotNull && column.RequireNullable {
				return contract{}, fail("SCHEMA_CONTRACT_INVALID")
			}
			columns[column.Name] = true
		}
		for _, unique := range table.Unique {
			if len(unique.Columns) == 0 || !validCondition(unique.RequiredWhen) {
				return contract{}, fail("SCHEMA_CONTRACT_INVALID")
			}
			for _, name := range unique.Columns {
				if !columns[name] {
					return contract{}, fail("SCHEMA_CONTRACT_INVALID")
				}
			}
		}
		for _, name := range table.PrimaryKey {
			if !columns[name] {
				return contract{}, fail("SCHEMA_CONTRACT_INVALID")
			}
		}
		for _, code := range table.Unknown {
			if !diagnosticCode.MatchString(code) {
				return contract{}, fail("SCHEMA_CONTRACT_INVALID")
			}
		}
	}
	for _, code := range c.Unknown {
		if !diagnosticCode.MatchString(code) {
			return contract{}, fail("SCHEMA_CONTRACT_INVALID")
		}
	}
	return c, nil
}

func validCondition(condition string) bool {
	switch condition {
	case "always", "if_present", "platform_catalog", "legacy_catalog", "observed_only":
		return true
	}
	return false
}

func required(condition string, platforms bool) bool {
	switch condition {
	case "always", "if_present":
		return true
	case "platform_catalog":
		return platforms
	case "legacy_catalog":
		return !platforms
	}
	return false
}
