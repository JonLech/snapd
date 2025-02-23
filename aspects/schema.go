// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2023 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package aspects

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/snapcore/snapd/strutil"
)

type parser interface {
	Schema

	// expectsConstraints returns true if the parser must have a map definition
	// with constraints or false, if it may have a simple name definition.
	expectsConstraints() bool

	// parseConstraints parses constraints for a type defined as a JSON object.
	// Shouldn't be used with non-object/map type definitions.
	parseConstraints(map[string]json.RawMessage) error
}

// ParseSchema parses a JSON aspect schema and returns a Schema that can be
// used to validate aspects.
func ParseSchema(raw []byte) (*StorageSchema, error) {
	var schemaDef map[string]json.RawMessage
	err := json.Unmarshal(raw, &schemaDef)
	if err != nil {
		return nil, fmt.Errorf("cannot parse top level schema as map: %w", err)
	}

	if rawType, ok := schemaDef["type"]; ok {
		var typ string
		if err := json.Unmarshal(rawType, &typ); err != nil {
			return nil, fmt.Errorf(`cannot parse top level schema's "type" entry: %w`, err)
		}

		if typ != "map" {
			return nil, fmt.Errorf(`cannot parse top level schema: unexpected declared type %q, should be "map" or omitted`, typ)
		}
	}

	if _, ok := schemaDef["schema"]; !ok {
		return nil, fmt.Errorf(`cannot parse top level schema: must have a "schema" constraint`)
	}

	schema := new(StorageSchema)
	if val, ok := schemaDef["types"]; ok {
		var userTypes map[string]json.RawMessage
		if err := json.Unmarshal(val, &userTypes); err != nil {
			return nil, fmt.Errorf(`cannot parse user-defined types map: %w`, err)
		}

		// TODO: if we want to allow user types to refer to others, this must be handled
		// explicitly since userTypes will not preserve any order in the serialized JSON
		schema.userTypes = make(map[string]*userTypeRefParser, len(userTypes))
		for userTypeName, typeDef := range userTypes {
			if !validUserType.Match([]byte(userTypeName)) {
				return nil, fmt.Errorf(`cannot parse user-defined type name %q: must match %s`, userTypeName, validUserType)
			}

			userTypeSchema, err := schema.parse(typeDef)
			if err != nil {
				return nil, fmt.Errorf(`cannot parse user-defined type %q: %w`, userTypeName, err)
			}

			schema.userTypes[userTypeName] = newUserTypeRefParser(userTypeSchema)
		}
	}

	schema.topLevel, err = schema.parse(raw)
	if err != nil {
		return nil, err
	}

	return schema, nil
}

// userTypeRefParser parses references to user-defined types (e.g., $my-type).
type userTypeRefParser struct {
	parser

	stringBased bool
}

func newUserTypeRefParser(p parser) *userTypeRefParser {
	_, ok := p.(*stringSchema)
	return &userTypeRefParser{
		parser:      p,
		stringBased: ok,
	}
}

// expectsConstraints return false because a reference to user type doesn't
// define constraints (these are defined under "types" at the top level).
func (*userTypeRefParser) expectsConstraints() bool {
	return false
}

// isStringBased returns true if this reference's base type is a string.
func (u *userTypeRefParser) isStringBased() bool {
	return u.stringBased
}

// StorageSchema represents an aspect schema and can be used to validate JSON
// aspects against it.
type StorageSchema struct {
	// topLevel is the schema for the top level map.
	topLevel Schema

	// userTypes contains schemas that can validate types defined by the user.
	userTypes map[string]*userTypeRefParser
}

// Validate validates the provided JSON object.
func (s *StorageSchema) Validate(raw []byte) error {
	return s.topLevel.Validate(raw)
}

func (s *StorageSchema) parse(raw json.RawMessage) (parser, error) {
	var typ string
	var schemaDef map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schemaDef); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return nil, fmt.Errorf(`cannot parse aspect schema: %w`, err)
		}

		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, fmt.Errorf(`cannot parse aspect schema: types constraint must be expressed as maps or strings: %w`, err)
		}
	} else {
		rawType, ok := schemaDef["type"]
		if !ok {
			typ = "map"
		} else {
			if err := json.Unmarshal(rawType, &typ); err != nil {
				return nil, fmt.Errorf(`cannot parse "type" constraint in type definition: %w`, err)
			}
		}
	}

	schema, err := s.newTypeSchema(typ)
	if err != nil {
		return nil, err
	}

	// only parse the schema if it's a schema definition w/ constraints
	if schemaDef != nil {
		if err := schema.parseConstraints(schemaDef); err != nil {
			return nil, err
		}
	} else if schema.expectsConstraints() {
		return nil, fmt.Errorf(`cannot parse %q: must be schema definition with constraints`, typ)
	}

	return schema, nil
}

func (s *StorageSchema) newTypeSchema(typ string) (parser, error) {
	switch typ {
	case "map":
		return &mapSchema{topSchema: s}, nil
	case "string":
		return &stringSchema{}, nil
	case "int":
		return &intSchema{}, nil
	case "any":
		return &anySchema{}, nil
	case "number":
		return &numberSchema{}, nil
	case "bool":
		return &booleanSchema{}, nil
	case "array":
		return &arraySchema{topSchema: s}, nil
	default:
		if typ != "" && typ[0] == '$' {
			return s.getUserType(typ[1:])
		}

		return nil, fmt.Errorf("cannot parse unknown type %q", typ)
	}
}

func (s *StorageSchema) getUserType(ref string) (*userTypeRefParser, error) {
	if userType, ok := s.userTypes[ref]; ok {
		return userType, nil
	}

	return nil, fmt.Errorf("cannot find user-defined type %q", ref)
}

type mapSchema struct {
	// topSchema is the schema for the top-level schema which contains the user types.
	topSchema *StorageSchema

	// entrySchemas maps keys to their expected types. Alternatively, the schema
	// can constrain key and/or value types.
	entrySchemas map[string]Schema

	// valueSchema validates that the map's values match a certain type.
	valueSchema Schema

	// keySchema validates that the map's key match a certain type.
	keySchema Schema

	// requiredCombs holds combinations of keys that an instance of the map is
	// allowed to have.
	requiredCombs [][]string
}

// Validate that raw is a valid aspect map and meets the constraints set by the
// aspect schema.
func (v *mapSchema) Validate(raw []byte) error {
	var mapValue map[string]json.RawMessage
	if err := json.Unmarshal(raw, &mapValue); err != nil {
		typeErr := &json.UnmarshalTypeError{}
		if errors.As(err, &typeErr) {
			return validationErrorf("expected map type but got %s", typeErr.Value)
		}
		return validationErrorFrom(err)
	}

	if mapValue == nil {
		return validationErrorf(`cannot accept null value for "map" type`)
	}

	if err := validMapKeys(mapValue); err != nil {
		return validationErrorFrom(err)
	}

	if v.entrySchemas != nil {
		for key := range mapValue {
			if _, ok := v.entrySchemas[key]; !ok {
				return validationErrorf(`map contains unexpected key %q`, key)
			}
		}
	}

	var missing bool
	for _, required := range v.requiredCombs {
		missing = false
		for _, key := range required {
			if _, ok := mapValue[key]; !ok {
				missing = true
				break
			}
		}

		if !missing {
			// matched possible combination of required keys so we can stop
			break
		}
	}

	if missing {
		return validationErrorf(`cannot find required combinations of keys`)
	}

	if v.entrySchemas != nil {
		for key, val := range mapValue {
			if validator, ok := v.entrySchemas[key]; ok {
				if err := validator.Validate(val); err != nil {
					var valErr *ValidationError
					if errors.As(err, &valErr) {
						valErr.Path = append([]interface{}{key}, valErr.Path...)
					}
					return err
				}
			}
		}

		// all required entries are present and validated
		return nil
	}

	if v.keySchema != nil {
		for k := range mapValue {
			rawKey, err := json.Marshal(k)
			if err != nil {
				return fmt.Errorf("internal error: %w", err)
			}

			if err := v.keySchema.Validate(rawKey); err != nil {
				var valErr *ValidationError
				if errors.As(err, &valErr) {
					valErr.Path = append([]interface{}{k}, valErr.Path...)
				}
				return err
			}
		}
	}

	if v.valueSchema != nil {
		for k, val := range mapValue {
			if err := v.valueSchema.Validate(val); err != nil {
				var valErr *ValidationError
				if errors.As(err, &valErr) {
					valErr.Path = append([]interface{}{k}, valErr.Path...)
				}
				return err
			}
		}
	}

	return nil
}

func validMapKeys(v map[string]json.RawMessage) error {
	for k := range v {
		if !validSubkey.Match([]byte(k)) {
			return fmt.Errorf(`key %q doesn't conform to required format`, k)
		}
	}

	return nil
}

func (v *mapSchema) parseConstraints(constraints map[string]json.RawMessage) error {
	err := checkExclusiveMapConstraints(constraints)
	if err != nil {
		return fmt.Errorf(`cannot parse map: %w`, err)
	}

	// maps can be "schemas" with types for specific entries and optional "required" constraints
	if rawEntries, ok := constraints["schema"]; ok {
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(rawEntries, &entries); err != nil {
			return fmt.Errorf(`cannot parse map's "schema" constraint: %v`, err)
		}

		if err := validMapKeys(entries); err != nil {
			return fmt.Errorf(`cannot parse map: %w`, err)
		}

		v.entrySchemas = make(map[string]Schema, len(entries))
		for key, value := range entries {
			entrySchema, err := v.topSchema.parse(value)
			if err != nil {
				return err
			}

			v.entrySchemas[key] = entrySchema
		}

		// "required" can be a list of keys or many lists of alternative combinations
		if rawRequired, ok := constraints["required"]; ok {
			var requiredCombs [][]string
			if err := json.Unmarshal(rawRequired, &requiredCombs); err != nil {
				var typeErr *json.UnmarshalTypeError
				if !errors.As(err, &typeErr) {
					return fmt.Errorf(`cannot parse map's "required" constraint: %v`, err)
				}

				var required []string
				if err := json.Unmarshal(rawRequired, &required); err != nil {
					return fmt.Errorf(`cannot parse map's "required" constraint: %v`, err)
				}

				v.requiredCombs = [][]string{required}
			} else {
				v.requiredCombs = requiredCombs
			}

			for _, requiredComb := range v.requiredCombs {
				for _, required := range requiredComb {
					if _, ok := v.entrySchemas[required]; !ok {
						return fmt.Errorf(`cannot parse map's "required" constraint: required key %q must have schema entry`, required)
					}
				}
			}
		}

		return nil
	}

	// map can not specify "schemas" and constrain the type of keys and values instead
	rawKeyDef, ok := constraints["keys"]
	if ok {
		if v.keySchema, err = v.parseMapKeyType(rawKeyDef); err != nil {
			return fmt.Errorf(`cannot parse "keys" constraint: %w`, err)
		}
	}

	rawValuesDef, ok := constraints["values"]
	if ok {
		v.valueSchema, err = v.topSchema.parse(rawValuesDef)
		if err != nil {
			return err
		}
	}

	if v.entrySchemas == nil && v.keySchema == nil && v.valueSchema == nil {
		return fmt.Errorf(`cannot parse map: must have "schema" or "keys"/"values" constraint`)
	}

	return nil
}

// checkExclusiveMapConstraints checks if the map contains mutually exclusive constraints.
func checkExclusiveMapConstraints(obj map[string]json.RawMessage) error {
	has := func(k string) bool {
		_, ok := obj[k]
		return ok
	}

	if has("required") && !has("schema") {
		return fmt.Errorf(`cannot use "required" without "schema" constraint`)
	}
	if has("schema") && has("keys") {
		return fmt.Errorf(`cannot use "schema" and "keys" constraints simultaneously`)
	}
	if has("schema") && has("values") {
		return fmt.Errorf(`cannot use "schema" and "values" constraints simultaneously`)
	}

	return nil
}

func (v *mapSchema) parseMapKeyType(raw json.RawMessage) (Schema, error) {
	var typ string
	if err := json.Unmarshal(raw, &typ); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return nil, err
		}

		var schemaDef map[string]json.RawMessage
		if err := json.Unmarshal(raw, &schemaDef); err != nil {
			return nil, err
		}

		if rawType, ok := schemaDef["type"]; ok {
			if err := json.Unmarshal(rawType, &typ); err != nil {
				return nil, err
			}

			if typ != "string" {
				return nil, fmt.Errorf(`must be based on string but got %q`, typ)
			}
		}

		schema := &stringSchema{}
		if err := schema.parseConstraints(schemaDef); err != nil {
			return nil, err
		}

		return schema, nil
	}

	if typ == "string" {
		return &stringSchema{}, nil
	}

	if typ != "" && typ[0] == '$' {
		userType, err := v.topSchema.getUserType(typ[1:])
		if err != nil {
			return nil, err
		}

		if !userType.isStringBased() {
			return nil, fmt.Errorf(`key type %q must be based on string`, typ[1:])
		}

		return userType, nil
	}

	return nil, fmt.Errorf(`keys must be based on string but got %q`, typ)
}

func (v *mapSchema) expectsConstraints() bool { return true }

type stringSchema struct {
	// pattern is a regex pattern that the string must match.
	pattern *regexp.Regexp

	// choices holds the possible values the string can take, if non-empty.
	choices []string
}

// Validate that raw is a valid aspect string and meets the schema's constraints.
func (v *stringSchema) Validate(raw []byte) (err error) {
	defer func() {
		if err != nil {
			err = validationErrorFrom(err)
		}
	}()

	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		typeErr := &json.UnmarshalTypeError{}
		if errors.As(err, &typeErr) {
			return fmt.Errorf("expected string type but got %s", typeErr.Value)
		}
		return err
	}

	if value == nil {
		return fmt.Errorf(`cannot accept null value for "string" type`)
	}

	if len(v.choices) != 0 && !strutil.ListContains(v.choices, *value) {
		return fmt.Errorf(`string %q is not one of the allowed choices`, *value)
	}

	if v.pattern != nil && !v.pattern.Match([]byte(*value)) {
		return fmt.Errorf(`string %q doesn't match schema pattern %s`, *value, v.pattern.String())
	}

	return nil
}

func (v *stringSchema) parseConstraints(constraints map[string]json.RawMessage) error {
	if rawChoices, ok := constraints["choices"]; ok {
		var choices []string
		if err := json.Unmarshal(rawChoices, &choices); err != nil {
			return fmt.Errorf(`cannot parse "choices" constraint: %w`, err)
		}

		if len(choices) == 0 {
			return fmt.Errorf(`cannot have a "choices" constraint with an empty list`)
		}

		v.choices = choices
	}

	if rawPattern, ok := constraints["pattern"]; ok {
		if v.choices != nil {
			return fmt.Errorf(`cannot use "choices" and "pattern" constraints in same schema`)
		}

		var patt string
		err := json.Unmarshal(rawPattern, &patt)
		if err != nil {
			return fmt.Errorf(`cannot parse "pattern" constraint: %w`, err)
		}

		if v.pattern, err = regexp.Compile(patt); err != nil {
			return fmt.Errorf(`cannot parse "pattern" constraint: %w`, err)
		}
	}

	return nil
}

func (v *stringSchema) expectsConstraints() bool { return false }

type intSchema struct {
	min     *int64
	max     *int64
	choices []int64
}

// Validate that raw is a valid integer and meets the schema's constraints.
func (v *intSchema) Validate(raw []byte) (err error) {
	defer func() {
		if err != nil {
			err = validationErrorFrom(err)
		}
	}()

	var num *int64
	if err := json.Unmarshal(raw, &num); err != nil {
		typeErr := &json.UnmarshalTypeError{}
		if errors.As(err, &typeErr) {
			return fmt.Errorf("expected int type but got %s", typeErr.Value)
		}
		return err
	}

	if num == nil {
		return fmt.Errorf(`cannot accept null value for "int" type`)
	}

	return validateNumber(*num, v.choices, v.min, v.max)
}

func (v *intSchema) parseConstraints(constraints map[string]json.RawMessage) error {
	if rawChoices, ok := constraints["choices"]; ok {
		var choices []int64
		err := json.Unmarshal(rawChoices, &choices)
		if err != nil {
			return fmt.Errorf(`cannot parse "choices" constraint: %v`, err)
		}

		if len(choices) == 0 {
			return fmt.Errorf(`cannot have "choices" constraint with empty list`)
		}

		v.choices = choices
	}

	if rawMin, ok := constraints["min"]; ok {
		if v.choices != nil {
			return fmt.Errorf(`cannot have "choices" and "min" constraints`)
		}

		var min int64
		if err := json.Unmarshal(rawMin, &min); err != nil {
			return fmt.Errorf(`cannot parse "min" constraint: %v`, err)
		}
		v.min = &min
	}

	if rawMax, ok := constraints["max"]; ok {
		if v.choices != nil {
			return fmt.Errorf(`cannot have "choices" and "max" constraints`)
		}

		var max int64
		if err := json.Unmarshal(rawMax, &max); err != nil {
			return fmt.Errorf(`cannot parse "max" constraint: %v`, err)
		}
		v.max = &max
	}

	if v.min != nil && v.max != nil && *v.min > *v.max {
		return fmt.Errorf(`cannot have "min" constraint with value greater than "max"`)
	}

	return nil
}

func (v *intSchema) expectsConstraints() bool { return false }

type anySchema struct{}

func (v *anySchema) Validate(raw []byte) (err error) {
	defer func() {
		if err != nil {
			err = validationErrorFrom(err)
		}
	}()

	var val interface{}
	if err := json.Unmarshal(raw, &val); err != nil {
		return err
	}

	if val == nil {
		return fmt.Errorf(`cannot accept null value for "any" type`)
	}
	return nil
}

func (v *anySchema) parseConstraints(constraints map[string]json.RawMessage) error {
	// no error because we're not explicitly rejecting unsupported keywords (for now)
	return nil
}

func (v *anySchema) expectsConstraints() bool { return false }

type numberSchema struct {
	min     *float64
	max     *float64
	choices []float64
}

// Validate that raw is a valid number and meets the schema's constraints.
func (v *numberSchema) Validate(raw []byte) (err error) {
	defer func() {
		if err != nil {
			err = validationErrorFrom(err)
		}
	}()

	var num *float64
	if err := json.Unmarshal(raw, &num); err != nil {
		typeErr := &json.UnmarshalTypeError{}
		if errors.As(err, &typeErr) {
			return fmt.Errorf("expected number type but got %s", typeErr.Value)
		}
		return err
	}

	if num == nil {
		return fmt.Errorf(`cannot accept null value for "number" type`)
	}

	return validateNumber(*num, v.choices, v.min, v.max)
}

func validateNumber[Num ~int64 | ~float64](num Num, choices []Num, min, max *Num) error {
	if len(choices) != 0 {
		var found bool
		for _, choice := range choices {
			if num == choice {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf(`%v is not one of the allowed choices`, num)
		}
	}

	// these comparisons are susceptible to floating-point errors but given that
	// this won't be used for general storage it should be precise enough
	if min != nil && num < *min {
		return fmt.Errorf(`%v is less than the allowed minimum %v`, num, *min)
	}

	if max != nil && num > *max {
		return fmt.Errorf(`%v is greater than the allowed maximum %v`, num, *max)
	}

	return nil
}

func (v *numberSchema) parseConstraints(constraints map[string]json.RawMessage) error {
	if rawChoices, ok := constraints["choices"]; ok {
		var choices []float64
		err := json.Unmarshal(rawChoices, &choices)
		if err != nil {
			return fmt.Errorf(`cannot parse "choices" constraint: %v`, err)
		}

		if len(choices) == 0 {
			return fmt.Errorf(`cannot have "choices" constraint with empty list`)
		}

		v.choices = choices
	}

	if rawMin, ok := constraints["min"]; ok {
		if v.choices != nil {
			return fmt.Errorf(`cannot have "choices" and "min" constraints`)
		}

		var min float64
		if err := json.Unmarshal(rawMin, &min); err != nil {
			return fmt.Errorf(`cannot parse "min" constraint: %v`, err)
		}
		v.min = &min
	}

	if rawMax, ok := constraints["max"]; ok {
		if v.choices != nil {
			return fmt.Errorf(`cannot have "choices" and "max" constraints`)
		}

		var max float64
		if err := json.Unmarshal(rawMax, &max); err != nil {
			return fmt.Errorf(`cannot parse "max" constraint: %v`, err)
		}
		v.max = &max
	}

	if v.min != nil && v.max != nil && *v.min > *v.max {
		return fmt.Errorf(`cannot have "min" constraint with value greater than "max"`)
	}

	return nil
}

func (v *numberSchema) expectsConstraints() bool { return false }

type booleanSchema struct{}

func (v *booleanSchema) Validate(raw []byte) (err error) {
	defer func() {
		if err != nil {
			err = validationErrorFrom(err)
		}
	}()

	var val *bool
	if err := json.Unmarshal(raw, &val); err != nil {
		typeErr := &json.UnmarshalTypeError{}
		if errors.As(err, &typeErr) {
			return fmt.Errorf("expected bool type but got %s", typeErr.Value)
		}
		return err
	}

	if val == nil {
		return fmt.Errorf(`cannot accept null value for "bool" type`)
	}

	return nil
}

func (v *booleanSchema) parseConstraints(map[string]json.RawMessage) error {
	// no error because we're not explicitly rejecting unsupported keywords (for now)
	return nil
}

func (v *booleanSchema) expectsConstraints() bool { return false }

type arraySchema struct {
	// topSchema is the schema for the top-level schema which contains the user types.
	topSchema *StorageSchema

	// elementType represents the type of the array's elements and can be used to
	// validate them.
	elementType Schema

	// unique is true if the array should not contain duplicates.
	unique bool
}

func (v *arraySchema) Validate(raw []byte) error {
	var array *[]json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil {
		typeErr := &json.UnmarshalTypeError{}
		if errors.As(err, &typeErr) {
			return validationErrorf("expected array type but got %s", typeErr.Value)
		}
		return validationErrorFrom(err)
	}

	if array == nil {
		return validationErrorf(`cannot accept null value for "array" type`)
	}

	for e, val := range *array {
		if err := v.elementType.Validate([]byte(val)); err != nil {
			var vErr *ValidationError
			if errors.As(err, &vErr) {
				vErr.Path = append([]interface{}{e}, vErr.Path...)
			}
			return err
		}
	}

	if v.unique {
		valSet := make(map[string]struct{}, len(*array))

		for _, val := range *array {
			encodedVal := string(val)
			if _, ok := valSet[encodedVal]; ok {
				return validationErrorf(`cannot accept duplicate values for array with "unique" constraint`)
			}
			valSet[encodedVal] = struct{}{}
		}
	}

	return nil
}

func (v *arraySchema) parseConstraints(constraints map[string]json.RawMessage) error {
	rawValues, ok := constraints["values"]
	if !ok {
		return fmt.Errorf(`cannot parse "array": must have "values" constraint`)
	}

	typ, err := v.topSchema.parse(rawValues)
	if err != nil {
		return fmt.Errorf(`cannot parse "array" values type: %v`, err)
	}

	v.elementType = typ

	if rawUnique, ok := constraints["unique"]; ok {
		var unique bool
		if err := json.Unmarshal(rawUnique, &unique); err != nil {
			return fmt.Errorf(`cannot parse array's "unique" constraint: %v`, err)
		}

		v.unique = unique
	}

	return nil
}

func (v *arraySchema) expectsConstraints() bool { return true }

type ValidationError struct {
	Path []interface{}
	Err  error
}

func (v *ValidationError) Error() string {
	var msg string
	if len(v.Path) == 0 {
		msg = "cannot accept top level element"
	} else {
		var sb strings.Builder
		for i, part := range v.Path {
			switch v := part.(type) {
			case string:
				if i > 0 {
					sb.WriteRune('.')
				}

				sb.WriteString(v)
			case int:
				sb.WriteString(fmt.Sprintf("[%d]", v))
			default:
				// can only happen due to bug
				sb.WriteString(".<n/a>")
			}
		}

		msg = fmt.Sprintf("cannot accept element in %q", sb.String())
	}

	return fmt.Sprintf("%s: %v", msg, v.Err)
}

func validationErrorFrom(err error) error {
	return &ValidationError{Err: err}
}

func validationErrorf(format string, v ...interface{}) error {
	return &ValidationError{Err: fmt.Errorf(format, v...)}
}
