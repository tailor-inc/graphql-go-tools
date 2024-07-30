package plan

import (
	"errors"
	"fmt"
	"slices"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
)

// nodeSelectionVisitor - walks through the operation multiple times to rewrite operation
// to be able to resolve fields from different datasources.
// During walks, it is adding required fields and rewrites abstract selection if it is necessary.
// We are revisiting query when we have:
// - new required fields were added to operation
// - when we have rewritten abstract field selection set
type nodeSelectionVisitor struct {
	debug DebugConfiguration

	operationName         string        // graphql query name
	operation, definition *ast.Document // graphql operation and schema documents
	walker                *astvisitor.Walker

	dataSources     []DataSource     // data sources configurations, which used by the current operation
	nodeSuggestions *NodeSuggestions // nodeSuggestions holds information about suggested data sources for each field

	selectionSetRefs []int // selectionSetRefs is a stack of selection set refs - used to add a required fields
	skipFieldsRefs   []int // skipFieldsRefs holds required field refs added by planner and should not be added to user response

	pendingKeyRequirements   map[int]pendingKeyRequirements   // pendingKeyRequirements is a map[selectionSetRef][]keyRequirements
	pendingFieldRequirements map[int]pendingFieldRequirements // pendingFieldRequirements is a map[selectionSetRef]fieldRequirements

	visitedFieldsRequiresChecks map[string]struct{}                       // visitedFieldsRequiresChecks is a map[FieldRef] of already processed fields which we check for presence of @requires directive
	visitedFieldsKeyChecks      map[string]struct{}                       // visitedFieldsKeyChecks is a map[FieldRef] of already processed fields which we check for @key requirements
	visitedFieldsAbstractChecks map[int]struct{}                          // visitedFieldsAbstractChecks is a map[FieldRef] of already processed fields which we check for abstract type, e.g. union or interface
	fieldDependsOn              map[string][]int                          // fieldDependsOn is a map[fieldRef][]fieldRef - holds list of field refs which are required by a field ref, e.g. field should be planned only after required fields were planned
	fieldRequirementsConfigs    map[string][]FederationFieldConfiguration // fieldRequirementsConfigs is a map[fieldRef]FederationFieldConfiguration - holds a list of required configuratuibs for a field ref to later built representation variables
	fieldLandedTo               map[int]DSHash                            // fieldLandedTo is a map[fieldRef]DSHash - holds a datasource hash where field was landed to

	secondaryRun        bool // secondaryRun is a flag to indicate that we're running the nodeSelectionVisitor not the first time
	hasNewFields        bool // hasNewFields is used to determine if we need to run the planner again. It will be true in case required fields were added
	hasUnresolvedFields bool // hasUnresolvedFields is used to determine if we need to run the planner again. We should set it to true in case we have unresolved fields
}

func (c *nodeSelectionVisitor) shouldRevisit() bool {
	return c.hasNewFields || c.hasUnresolvedFields
}

// selectionSetPendingRequirements - is a wrapper to been able to have predictable order of keyRequirements but at the same time deduplicate keyRequirements
type pendingKeyRequirements struct {
	existsTracker      map[DSHash]struct{} // existsTracker allows us to not add duplicated keyRequirements
	parentDSHashes     []DSHash            // parentDSHashes holds a list of parent data sources hashes
	requirementConfigs []keyRequirements   // requirementConfigs is a list of keyRequirements which should be added to the selection set
}

// keyRequirements is a mapping between requestedByPlannerID or requestedByFieldRef, which requested required fields,
// and selectionSet which should be added
type keyRequirements struct {
	dsHash               DSHash
	path                 string
	isInterfaceObject    bool
	possibleKeys         []FederationFieldConfiguration
	requestedByFieldRefs []int
}

type fieldRequirements struct {
	dsHash               DSHash
	path                 string
	selectionSet         string
	requestedByFieldRefs []int
}

type pendingFieldRequirements struct {
	existsTracker      map[string]struct{} // existsTracker allows us to not add duplicated fieldRequirements
	requirementConfigs []fieldRequirements // requirementConfigs is a list of fieldRequirements which should be added to the selection set
}

func (c *nodeSelectionVisitor) currentSelectionSet() int {
	if len(c.selectionSetRefs) == 0 {
		return ast.InvalidRef
	}

	return c.selectionSetRefs[len(c.selectionSetRefs)-1]
}

func (c *nodeSelectionVisitor) debugPrint(args ...any) {
	if !c.debug.ConfigurationVisitor {
		return
	}

	printArgs := []any{"[nodeSelectionVisitor]: "}
	printArgs = append(printArgs, args...)
	fmt.Println(printArgs...)
}

func (c *nodeSelectionVisitor) EnterDocument(operation, definition *ast.Document) {
	c.hasNewFields = false
	c.hasUnresolvedFields = false

	if c.selectionSetRefs == nil {
		c.selectionSetRefs = make([]int, 0, 8)
	} else {
		c.selectionSetRefs = c.selectionSetRefs[:0]
	}

	if c.secondaryRun {
		return
	}

	c.operation, c.definition = operation, definition

	if c.skipFieldsRefs == nil {
		c.skipFieldsRefs = make([]int, 0, 8)
	}

	c.visitedFieldsAbstractChecks = make(map[int]struct{})
	c.visitedFieldsRequiresChecks = make(map[string]struct{})
	c.visitedFieldsKeyChecks = make(map[string]struct{})
	c.pendingKeyRequirements = make(map[int]pendingKeyRequirements)
	c.pendingFieldRequirements = make(map[int]pendingFieldRequirements)

	c.fieldDependsOn = make(map[string][]int)
	c.fieldRequirementsConfigs = make(map[string][]FederationFieldConfiguration)
	c.fieldLandedTo = make(map[int]DSHash)
}

func (c *nodeSelectionVisitor) LeaveDocument(operation, definition *ast.Document) {

}

func (c *nodeSelectionVisitor) EnterOperationDefinition(ref int) {
	operationName := c.operation.OperationDefinitionNameString(ref)
	if c.operationName != operationName {
		c.walker.SkipNode()
		return
	}
}

func (c *nodeSelectionVisitor) EnterSelectionSet(ref int) {
	c.debugPrint("EnterSelectionSet ref:", ref)
	c.selectionSetRefs = append(c.selectionSetRefs, ref)
}

func (c *nodeSelectionVisitor) LeaveSelectionSet(ref int) {
	c.debugPrint("LeaveSelectionSet ref:", ref)
	c.processPendingFieldRequirements(ref)
	c.processPendingKeyRequirements(ref)
	c.selectionSetRefs = c.selectionSetRefs[:len(c.selectionSetRefs)-1]
}

func (c *nodeSelectionVisitor) EnterField(fieldRef int) {
	root := c.walker.Ancestors[0]
	if root.Kind != ast.NodeKindOperationDefinition {
		return
	}

	fieldName := c.operation.FieldNameUnsafeString(fieldRef)
	fieldAliasOrName := c.operation.FieldAliasOrNameString(fieldRef)
	typeName := c.walker.EnclosingTypeDefinition.NameString(c.definition)

	c.debugPrint("EnterField ref:", fieldRef, "fieldName:", fieldName, "typeName:", typeName)

	parentPath := c.walker.Path.DotDelimitedString()
	currentPath := parentPath + "." + fieldAliasOrName

	suggestions := c.nodeSuggestions.SuggestionsForPath(typeName, fieldName, currentPath)

	for _, suggestion := range suggestions {
		// TODO: change SuggestionsForPath to return only selected suggestions
		if !suggestion.Selected {
			continue
		}

		if suggestion.IsRequiredKeyField {
			// it was already selected as a key field
			// no need to process required fields for it
			continue
		}

		dsIdx := slices.IndexFunc(c.dataSources, func(d DataSource) bool {
			return d.Hash() == suggestion.DataSourceHash
		})
		if dsIdx == -1 {
			c.walker.StopWithInternalErr(errors.New("we should always have a datasource for a suggestion"))
			return
		}
		ds := c.dataSources[dsIdx]

		// check if the field has @requires directive
		c.handleFieldRequiredByRequires(fieldRef, parentPath, typeName, fieldName, currentPath, ds)

		// check key requirements for the field
		c.handleFieldsRequiredByKey(fieldRef, parentPath, typeName, fieldName, currentPath, ds)

		// check if a field type is abstract and need rewrites
		c.rewriteSelectionSetOfFieldWithInterfaceType(fieldRef, ds)
	}
}

func (c *nodeSelectionVisitor) LeaveField(ref int) {
}

func (c *nodeSelectionVisitor) handleFieldRequiredByRequires(fieldRef int, parentPath, typeName, fieldName, currentPath string, dsConfig DataSource) {
	fieldKey := fmt.Sprintf("%d.%d", fieldRef, dsConfig.Hash())
	_, visited := c.visitedFieldsRequiresChecks[fieldKey]
	if visited {
		return
	}
	c.visitedFieldsRequiresChecks[fieldKey] = struct{}{}

	if fieldName == typeNameField {
		// the __typename field could not have @requires directive
		return
	}

	requiresConfiguration, exists := dsConfig.RequiredFieldsByRequires(typeName, fieldName)
	if !exists {
		// we do not have a @requires configuration for the field
		return
	}

	// we should plan adding required fields for the field
	// they will be added in the on LeaveSelectionSet callback for the current selection set
	// and current field ref will be added to fieldDependsOn map
	c.addPendingFieldRequirements(fieldRef, dsConfig.Hash(), requiresConfiguration, currentPath)
	c.hasNewFields = true
}

func (c *nodeSelectionVisitor) handleFieldsRequiredByKey(fieldRef int, parentPath, typeName, fieldName, currentPath string, dsConfig DataSource) {
	fieldKey := fmt.Sprintf("%d.%d", fieldRef, dsConfig.Hash())
	_, visited := c.visitedFieldsKeyChecks[fieldKey]
	if visited {
		return
	}
	c.visitedFieldsKeyChecks[fieldKey] = struct{}{}

	_, hasRequiresCondition := dsConfig.RequiredFieldsByRequires(typeName, fieldName)

	treeNodeID := TreeNodeID(fieldRef)
	treeNode, ok := c.nodeSuggestions.responseTree.Find(treeNodeID)
	if !ok {
		return
	}

	// TODO: refactor
	parent := treeNode.GetParent()
	parentSuggestions := parent.GetData()
	var selectedParentsDSHashes []DSHash
	for _, itemID := range parentSuggestions {
		if c.nodeSuggestions.items[itemID].Selected {
			selectedParentsDSHashes = append(selectedParentsDSHashes, c.nodeSuggestions.items[itemID].DataSourceHash)
		}
	}

	entityInterface := dsConfig.HasEntityInterface(typeName)
	interfaceObject := dsConfig.HasInterfaceObject(typeName)

	if fieldName == typeNameField && !entityInterface {
		// the __typename field could not have @key directive
		// but for the interface object we have to plan it differently
		// e.g. we should get a __typename from a concrete type, not the interface object
		// it means for the entity interface we should evaluate key deps on a __typename field
		return
	}

	// we should handle key requirements only when the datasource hash differs from the parent datasource hash
	// it means that this field should be resolved by another datasource
	// one exception in case field has requires directive - then field is planned on the same datasource
	// but fields with requires waits for the required fields to be resolved
	sameAsParentDS := len(selectedParentsDSHashes) == 1 && selectedParentsDSHashes[0] == dsConfig.Hash()

	if sameAsParentDS && !hasRequiresCondition {
		return
	}

	keyConfigurations := dsConfig.RequiredFieldsByKey(typeName)

	if len(keyConfigurations) == 0 && hasRequiresCondition {
		// required fields could be of zero length in case type is not entity
		// or when entity has disabled entity resolver.
		// Usually we can't jump to the entity with disabled entity resolver, but there is one known exception
		// When entity has disabled entity resolver, but we have field with requires directive on this entity
		// we should add key fields for the field with requires - to pass them into field resolver

		keys := dsConfig.FederationConfiguration().Keys
		keyConfigurations = keys.FilterByTypeAndResolvability(typeName, false)
	}

	if len(keyConfigurations) == 0 && !sameAsParentDS {
		// TODO: planner error
		return
	}

	// 1. Current field datasource is the same as parent datasource, and field has requires directive defined
	if sameAsParentDS {
		// the most simple case we just need to use the first available key configuration
		c.addPendingKeyRequirements(fieldRef, dsConfig.Hash(), []FederationFieldConfiguration{keyConfigurations[0]}, false, parentPath, selectedParentsDSHashes)
		c.hasNewFields = true
		return
	}

	c.addPendingKeyRequirements(fieldRef, dsConfig.Hash(), keyConfigurations, interfaceObject, parentPath, selectedParentsDSHashes)
	c.hasNewFields = true
}

func (c *nodeSelectionVisitor) addPendingFieldRequirements(requestedByFieldRef int, dsHash DSHash, fieldConfiguration FederationFieldConfiguration, currentPath string) {
	currentSelectionSet := c.currentSelectionSet()

	requirements, hasRequirements := c.pendingFieldRequirements[currentSelectionSet]
	if !hasRequirements {
		requirements = pendingFieldRequirements{
			existsTracker: make(map[string]struct{}),
		}
	}

	existsKey := fmt.Sprintf("%d.%s", dsHash, fieldConfiguration.SelectionSet)
	if _, exists := requirements.existsTracker[existsKey]; !exists {
		config := fieldRequirements{
			dsHash:               dsHash,
			path:                 currentPath,
			selectionSet:         fieldConfiguration.SelectionSet,
			requestedByFieldRefs: []int{requestedByFieldRef},
		}

		requirements.existsTracker[existsKey] = struct{}{}
		requirements.requirementConfigs = append(requirements.requirementConfigs, config)
	} else {
		for i := range requirements.requirementConfigs {
			if requirements.requirementConfigs[i].selectionSet == fieldConfiguration.SelectionSet {
				if slices.IndexFunc(requirements.requirementConfigs[i].requestedByFieldRefs, func(fieldRef int) bool {
					return fieldRef == requestedByFieldRef
				}) == -1 {
					requirements.requirementConfigs[i].requestedByFieldRefs = append(requirements.requirementConfigs[i].requestedByFieldRefs, requestedByFieldRef)
				}
				break
			}
		}
	}

	c.pendingFieldRequirements[currentSelectionSet] = requirements
	fieldKey := fmt.Sprintf("%d.%d", requestedByFieldRef, dsHash)
	c.fieldRequirementsConfigs[fieldKey] = append(c.fieldRequirementsConfigs[fieldKey], fieldConfiguration)
}

func (c *nodeSelectionVisitor) addPendingKeyRequirements(requestedByFieldRef int, dsHash DSHash, possibleFieldConfigurations []FederationFieldConfiguration, isInterfaceObject bool, parentPath string, parentDSHashes []DSHash) {
	currentSelectionSet := c.currentSelectionSet()

	requirements, hasRequirements := c.pendingKeyRequirements[currentSelectionSet]

	if !hasRequirements {
		requirements = pendingKeyRequirements{
			existsTracker:  make(map[DSHash]struct{}),
			parentDSHashes: parentDSHashes,
		}
	}

	existsKey := dsHash
	if _, exists := requirements.existsTracker[existsKey]; !exists {
		config := keyRequirements{
			dsHash:               dsHash,
			path:                 parentPath,
			isInterfaceObject:    isInterfaceObject,
			possibleKeys:         possibleFieldConfigurations,
			requestedByFieldRefs: []int{requestedByFieldRef},
		}

		requirements.existsTracker[existsKey] = struct{}{}
		requirements.requirementConfigs = append(requirements.requirementConfigs, config)
	} else {
		for i := range requirements.requirementConfigs {
			if requirements.requirementConfigs[i].dsHash == dsHash {
				if slices.Index(requirements.requirementConfigs[i].requestedByFieldRefs, requestedByFieldRef) == -1 {
					requirements.requirementConfigs[i].requestedByFieldRefs = append(requirements.requirementConfigs[i].requestedByFieldRefs, requestedByFieldRef)
				}
				break
			}
		}
	}

	c.pendingKeyRequirements[currentSelectionSet] = requirements
}

func (c *nodeSelectionVisitor) processPendingFieldRequirements(selectionSetRef int) {
	configs, hasSelectionSet := c.pendingFieldRequirements[selectionSetRef]
	if !hasSelectionSet {
		return
	}
	delete(c.pendingFieldRequirements, selectionSetRef)

	for _, requiredFieldsCfg := range configs.requirementConfigs {
		c.addFieldRequirementsToOperation(selectionSetRef, requiredFieldsCfg)
	}
}

func (c *nodeSelectionVisitor) addFieldRequirementsToOperation(selectionSetRef int, requirements fieldRequirements) {
	typeName := c.walker.EnclosingTypeDefinition.NameString(c.definition)
	key, report := RequiredFieldsFragment(typeName, requirements.selectionSet, false)
	if report.HasErrors() {
		c.walker.StopWithInternalErr(fmt.Errorf("failed to parse required fields %s for %s at path %s", requirements.selectionSet, typeName, requirements.path))
		return
	}

	input := &addRequiredFieldsInput{
		key:                   key,
		operation:             c.operation,
		definition:            c.definition,
		report:                report,
		operationSelectionSet: selectionSetRef,
	}

	skipFieldRefs, requiredFieldRefs := addRequiredFields(input)
	if report.HasErrors() {
		c.walker.StopWithInternalErr(fmt.Errorf("failed to add required fields %s for %s at path %s", requirements.selectionSet, typeName, requirements.path))
		return
	}

	c.skipFieldsRefs = append(c.skipFieldsRefs, skipFieldRefs...)
	// add mapping for the field dependencies
	for _, requestedByFieldRef := range requirements.requestedByFieldRefs {
		fieldKey := fmt.Sprintf("%d.%d", requestedByFieldRef, requirements.dsHash)
		c.fieldDependsOn[fieldKey] = append(c.fieldDependsOn[fieldKey], requiredFieldRefs...)
	}
}

func (c *nodeSelectionVisitor) processPendingKeyRequirements(selectionSetRef int) {
	configs, hasSelectionSet := c.pendingKeyRequirements[selectionSetRef]
	if !hasSelectionSet {
		return
	}
	delete(c.pendingKeyRequirements, selectionSetRef)

	availableHashes := configs.parentDSHashes

	pendingRequirements := configs.requirementConfigs
	hasPendingRequirements := len(pendingRequirements) > 0

	for hasPendingRequirements {
		newAvailableHashes := make([]DSHash, 0, len(availableHashes))
		newPendingRequirements := make([]keyRequirements, 0, len(pendingRequirements))
		for i := 0; i < len(pendingRequirements); i++ {
			if c.matchDataSourcesByKeyConfiguration(selectionSetRef, pendingRequirements[i], availableHashes) {
				newAvailableHashes = append(newAvailableHashes, pendingRequirements[i].dsHash)
			} else {
				newPendingRequirements = append(newPendingRequirements, pendingRequirements[i])
			}
		}
		availableHashes = newAvailableHashes
		hasPendingRequirements = len(newPendingRequirements) > 0
		pendingRequirements = newPendingRequirements
	}
}

func (c *nodeSelectionVisitor) matchDataSourcesByKeyConfiguration(selectionSetRef int, requirements keyRequirements, dsHashes []DSHash) (matched bool) {
	for _, ds := range c.dataSources {
		if !slices.Contains(dsHashes, ds.Hash()) {
			continue
		}

		for _, possibleRequiredFieldConfig := range requirements.possibleKeys {
			typeName := possibleRequiredFieldConfig.TypeName
			if ds.HasKeyRequirement(typeName, possibleRequiredFieldConfig.SelectionSet) {
				c.addKeyRequirementsToOperation(selectionSetRef, typeName, requirements, ds, possibleRequiredFieldConfig)

				return true
			}
		}
	}

	return false
}

func (c *nodeSelectionVisitor) addKeyRequirementsToOperation(selectionSetRef int, typeName string, requirements keyRequirements, landedTo DataSource, fieldConfiguration FederationFieldConfiguration) {
	requirementsFromInterfaceObject := requirements.isInterfaceObject
	requirementsToInterfaceObject := landedTo.HasInterfaceObject(typeName)

	// when we jump from interface object to interface object, we don't need a concrete type __typename to do the jump,
	// so we have to skip adding __typename field along with other key fields
	dissalowTypeName := requirementsFromInterfaceObject && requirementsToInterfaceObject

	key, report := RequiredFieldsFragment(typeName, fieldConfiguration.SelectionSet, !dissalowTypeName)
	if report.HasErrors() {
		c.walker.StopWithInternalErr(fmt.Errorf("failed to parse required fields %s for %s at path %s", fieldConfiguration.SelectionSet, typeName, requirements.path))
		return
	}

	input := &addRequiredFieldsInput{
		key:                   key,
		operation:             c.operation,
		definition:            c.definition,
		report:                report,
		operationSelectionSet: selectionSetRef,
	}

	skipFieldRefs, requiredFieldRefs := addRequiredFields(input)
	if report.HasErrors() {
		c.walker.StopWithInternalErr(fmt.Errorf("failed to add required fields %s for %s at path %s", fieldConfiguration.SelectionSet, typeName, requirements.path))
		return
	}

	c.skipFieldsRefs = append(c.skipFieldsRefs, skipFieldRefs...)

	// add mapping for the field dependencies
	for _, requestedByFieldRef := range requirements.requestedByFieldRefs {
		if slices.Contains(requiredFieldRefs, requestedByFieldRef) {
			// we should not add field ref to fieldDependsOn map if it is part of a key
			continue
		}

		fieldKey := fmt.Sprintf("%d.%d", requestedByFieldRef, requirements.dsHash)
		c.fieldDependsOn[fieldKey] = append(c.fieldDependsOn[fieldKey], requiredFieldRefs...)
		c.fieldRequirementsConfigs[fieldKey] = append(c.fieldRequirementsConfigs[fieldKey], fieldConfiguration)
	}

	for _, requiredFieldRef := range requiredFieldRefs {
		c.fieldLandedTo[requiredFieldRef] = landedTo.Hash()
	}
}

func (c *nodeSelectionVisitor) rewriteSelectionSetOfFieldWithInterfaceType(fieldRef int, ds DataSource) {
	if _, ok := c.visitedFieldsAbstractChecks[fieldRef]; ok {
		return
	}
	c.visitedFieldsAbstractChecks[fieldRef] = struct{}{}

	upstreamSchema, ok := ds.UpstreamSchema()
	if !ok {
		return
	}

	rewriter := newFieldSelectionRewriter(c.operation, c.definition)
	rewriter.SetUpstreamDefinition(upstreamSchema)
	rewriter.SetDatasourceConfiguration(ds)

	rewritten, err := rewriter.RewriteFieldSelection(fieldRef, c.walker.EnclosingTypeDefinition)

	if err != nil {
		c.walker.StopWithInternalErr(err)
		return
	}

	if !rewritten {
		return
	}

	c.hasNewFields = true
	c.walker.Stop()
}
