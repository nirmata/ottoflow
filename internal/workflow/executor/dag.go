/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"fmt"
	"sort"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// DAG represents a directed acyclic graph of workflow steps
type DAG struct {
	nodes map[string]*Node
	edges map[string][]string // stepName -> []dependentStepNames
}

// Node represents a step in the DAG
type Node struct {
	Name         string
	Step         ottoflowv1alpha1.Step
	Dependencies []string // step names this node depends on
	Dependents   []string // step names that depend on this node
}

// NewDAG creates a new DAG
func NewDAG() *DAG {
	return &DAG{
		nodes: make(map[string]*Node),
		edges: make(map[string][]string),
	}
}

// AddNode adds a step node to the DAG
func (d *DAG) AddNode(step ottoflowv1alpha1.Step) {
	if d.nodes[step.Name] == nil {
		d.nodes[step.Name] = &Node{
			Name:         step.Name,
			Step:         step,
			Dependencies: []string{},
			Dependents:   []string{},
		}
	}
}

// AddEdge adds a dependency edge: fromStep -> toStep (toStep depends on fromStep)
func (d *DAG) AddEdge(fromStep, toStep string) {
	if d.nodes[fromStep] == nil || d.nodes[toStep] == nil {
		return
	}

	// Add to dependents of fromStep
	found := false
	for _, dep := range d.nodes[fromStep].Dependents {
		if dep == toStep {
			found = true
			break
		}
	}
	if !found {
		d.nodes[fromStep].Dependents = append(d.nodes[fromStep].Dependents, toStep)
	}

	// Add to dependencies of toStep
	found = false
	for _, dep := range d.nodes[toStep].Dependencies {
		if dep == fromStep {
			found = true
			break
		}
	}
	if !found {
		d.nodes[toStep].Dependencies = append(d.nodes[toStep].Dependencies, fromStep)
	}

	// Track edges
	d.edges[fromStep] = append(d.edges[fromStep], toStep)
}

// GetNode returns a node by name
func (d *DAG) GetNode(name string) *Node {
	return d.nodes[name]
}

// GetAllNodes returns all nodes
func (d *DAG) GetAllNodes() []*Node {
	nodes := make([]*Node, 0, len(d.nodes))
	for _, node := range d.nodes {
		nodes = append(nodes, node)
	}
	return nodes
}

// HasCycle detects if the DAG contains cycles using DFS
func (d *DAG) HasCycle() bool {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	for name := range d.nodes {
		if !visited[name] {
			if d.hasCycleDFS(name, visited, recStack) {
				return true
			}
		}
	}
	return false
}

func (d *DAG) hasCycleDFS(nodeName string, visited, recStack map[string]bool) bool {
	visited[nodeName] = true
	recStack[nodeName] = true

	node := d.nodes[nodeName]
	for _, dependent := range node.Dependents {
		if !visited[dependent] {
			if d.hasCycleDFS(dependent, visited, recStack) {
				return true
			}
		} else if recStack[dependent] {
			return true
		}
	}

	recStack[nodeName] = false
	return false
}

// GetReadySteps returns steps that have all dependencies satisfied
func (d *DAG) GetReadySteps(completedSteps map[string]bool) []string {
	var ready []string

	for name, node := range d.nodes {
		// Skip if already completed
		if completedSteps[name] {
			continue
		}

		// Check if all dependencies are completed
		allDepsCompleted := true
		for _, dep := range node.Dependencies {
			if !completedSteps[dep] {
				allDepsCompleted = false
				break
			}
		}

		if allDepsCompleted {
			ready = append(ready, name)
		}
	}

	// Map iteration order is random; sort so execution order and maxConcurrentSteps truncation are deterministic.
	sort.Strings(ready)
	return ready
}

// BuildDAG constructs a DAG from workflow steps
// Dependencies are determined solely by explicit dependsOn declarations
// No dependency inference is performed - all dependencies must be explicitly declared
func BuildDAG(steps []ottoflowv1alpha1.Step) (*DAG, error) {
	dag := NewDAG()

	// Step 1: Add all steps as nodes
	for _, step := range steps {
		dag.AddNode(step)
	}

	// Step 2: Process explicit dependencies (dependsOn field)
	// This is the only way dependencies are determined - no inference
	for _, step := range steps {
		if len(step.DependsOn) > 0 {
			for _, depName := range step.DependsOn {
				if dag.GetNode(depName) == nil {
					return nil, fmt.Errorf("step %s depends on non-existent step %s", step.Name, depName)
				}
				dag.AddEdge(depName, step.Name)
			}
		}
	}

	// Step 3: Detect cycles
	if dag.HasCycle() {
		return nil, fmt.Errorf("workflow contains circular dependencies")
	}

	return dag, nil
}
