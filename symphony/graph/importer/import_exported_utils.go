// Copyright (c) 2004-present Facebook All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package importer

import (
	"context"
	"fmt"
	"strings"

	"github.com/facebookincubator/symphony/graph/ent"
	"github.com/facebookincubator/symphony/graph/ent/equipment"
	"github.com/facebookincubator/symphony/graph/ent/equipmentposition"
	"github.com/facebookincubator/symphony/graph/ent/equipmentpositiondefinition"
	"github.com/facebookincubator/symphony/graph/ent/locationtype"
	"github.com/facebookincubator/symphony/graph/graphql/models"
	"github.com/pkg/errors"
)

// ImportEntity specifies an entity that can be imported
type ImportEntity string

const (
	// ImportEntityEquipment specifies an equipment for import
	ImportEntityEquipment ImportEntity = "EQUIPMENT"
	// ImportEntityPort specifies a port for import
	ImportEntityPort ImportEntity = "PORT"
	// ImportEntityLink specifies a link for import
	ImportEntityLink ImportEntity = "LINK"
	// ImportEntityService specifies a service for import
	ImportEntityService ImportEntity = "SERVICE"
)

// nolint: unparam
func (m *importer) validateAllLocationTypeExist(ctx context.Context, offset int, locations []string, ignoreHierarchy bool) error {
	currIndex := -1
	ic := getImportContext(ctx)
	for i, locName := range locations {
		lt, err := m.ClientFrom(ctx).LocationType.Query().Where(locationtype.Name(locName)).Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				return errors.New("location type not found, create it: + " + locName)
			}
			return err
		}
		if !ignoreHierarchy {
			if currIndex >= lt.Index {
				return errors.New("location types are not in the right order on the first line. edit the index and export again")
			}
			currIndex = lt.Index
		}
		ic.indexToLocationTypeID[offset+i] = lt.ID
	}
	return nil
}

// nolint: unparam
func (m *importer) verifyOrCreateLocationHierarchy(ctx context.Context, l ImportRecord) (*ent.Location, error) {
	var currParentID *string
	var loc *ent.Location
	ic := getImportContext(ctx)
	locStart, _ := l.Header().LocationsRangeIdx()
	for i, locName := range l.LocationsRangeArr() {
		if locName == "" {
			continue
		}
		typID := ic.indexToLocationTypeID[i+locStart] // the actual index
		typ, err := m.r.Query().LocationType(ctx, typID)
		if err != nil {
			return nil, errors.Wrapf(err, "missing location type: id=%q", typID)
		}
		loc, _ = m.getOrCreateLocation(ctx, locName, 0.0, 0.0, typ, currParentID, nil, nil)
		currParentID = &loc.ID
	}
	if loc == nil {
		return nil, errors.Errorf("equipment with no locations specified. id:%q, name: %q", l.ID(), l.Name())
	}
	return loc, nil
}

func (m *importer) validateLocationHierarchy(ctx context.Context, equipment *ent.Equipment, importLine ImportRecord) error {
	locs, err := m.r.Equipment().LocationHierarchy(ctx, equipment)
	if err != nil {
		return errors.Wrapf(err, "fetching location hierarchy")
	}
	prevIdx := 0
	for _, loc := range locs {
		currIdx := findIndex(importLine.line, strings.Trim(loc.Name, " "))
		if currIdx == -1 {
			return errors.Errorf("missing location from hierarchy (%q)", loc.Name)
		}
		if prevIdx > currIdx {
			return errors.Errorf("location not in the right order (%q)", loc.Name)
		}
		prevIdx = currIdx
	}
	return nil
}

func (m *importer) verifyPositionHierarchy(ctx context.Context, equipment *ent.Equipment, importLine ImportRecord) error {
	posHierarchy, err := m.r.Equipment().PositionHierarchy(ctx, equipment)
	if err != nil {
		return errors.Wrapf(err, "fetching positions hierarchy for equipment")
	}
	length := len(posHierarchy)
	if length > 0 {
		if length > 4 {
			// getting the last 4 positions (we currently support 4 on export)
			posHierarchy = posHierarchy[(length - 4):]
		}
		directPos := posHierarchy[length-1]

		defName := directPos.QueryDefinition().OnlyX(ctx).Name
		if defName != importLine.Position() {
			return errors.Errorf("wrong position name. should be %v, but %v", importLine.Position(), defName)
		}
		pName := directPos.QueryParent().OnlyX(ctx).Name
		if pName != importLine.DirectParent() {
			return errors.Errorf("wrong equipment parent name. should be %v, but %v", importLine.DirectParent(), pName)
		}
	}
	return nil
}

func (m *importer) getPositionDetailsIfExists(ctx context.Context, parentLoc *ent.Location, importLine ImportRecord) (*string, *string, error) {
	l := importLine.line
	title := importLine.title
	if importLine.Position() == "" {
		return nil, nil, nil
	}
	var (
		equip        *ent.Equipment
		err          error
		errMsg       error
		positionName string
	)
	for idx := title.prnt3Idx; idx < title.PositionIdx(); idx += 2 {
		if l[idx] == "" {
			continue
		}
		if equip == nil {
			equip, err = parentLoc.QueryEquipment().Where(equipment.Name(l[idx])).Only(ctx)
			errMsg = fmt.Errorf("equipment %q not found under location %q", l[idx], parentLoc.Name)
		} else {
			equip, err = equip.QueryPositions().
				Where(equipmentposition.HasDefinitionWith(equipmentpositiondefinition.Name(positionName))).
				QueryAttachment().
				Where(equipment.Name(l[idx])).Only(ctx)
			errMsg = fmt.Errorf("position %q not found under equipment %q", positionName, l[idx])
		}
		positionName = l[idx+1]
		if err != nil {
			if ent.IsNotFound(err) {
				return nil, nil, errMsg
			}
			return nil, nil, err
		}
	}
	if equip == nil {
		return nil, nil, errors.Errorf("location/equipment/position mismatch %q, %q, %q", parentLoc.Name, importLine.DirectParent(), importLine.Position())
	}
	def, err := equip.QueryType().QueryPositionDefinitions().Where(equipmentpositiondefinition.Name(importLine.Position())).Only(ctx)
	if err != nil {
		return nil, nil, err
	}
	hasAttachment, err := equip.QueryPositions().
		Where(equipmentposition.HasDefinitionWith(equipmentpositiondefinition.ID(def.ID))).
		QueryAttachment().
		Exist(ctx)
	if err != nil {
		return nil, nil, err
	}
	if hasAttachment {
		return nil, nil, errors.Errorf("position %q already has attachment", importLine.Position())
	}
	return &equip.ID, &def.ID, nil
}

func (m *importer) validatePropertiesForPortType(ctx context.Context, line ImportRecord, portType *ent.EquipmentPortType, entity ImportEntity) ([]*models.PropertyInput, error) {
	var pInputs []*models.PropertyInput
	var propTypes []*ent.PropertyType
	var err error
	switch entity {
	case ImportEntityPort:
		propTypes, err = portType.QueryPropertyTypes().All(ctx)
	case ImportEntityLink:
		propTypes, err = portType.QueryLinkPropertyTypes().All(ctx)
	default:
		return nil, errors.New(fmt.Sprintf("ImportEntity not supported %s", entity))
	}
	if ent.MaskNotFound(err) != nil {
		return nil, errors.Wrap(err, "can't query property types for port type")
	}
	for _, ptype := range propTypes {
		ptypeName := ptype.Name
		pInput, err := line.GetPropertyInput(m.ClientFrom(ctx), ctx, portType, ptypeName)
		if err != nil {
			return nil, err
		}
		pInputs = append(pInputs, pInput)
	}
	return pInputs, nil
}
