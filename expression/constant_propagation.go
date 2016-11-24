// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/util/types"
)

// MaxPropagateColsCnt means the max number of columns that can participate propagation.
var MaxPropagateColsCnt = 100

var eqFuncNameMap = map[string]bool{
	ast.EQ: true,
}

// inEqFuncNameMap stores all the in-equal operators that can be propagated.
var inEqFuncNameMap = map[string]bool{
	ast.LT: true,
	ast.GT: true,
	ast.LE: true,
	ast.GE: true,
	ast.NE: true,
}

type propagateConstantSolver struct {
	colMapper        map[string]int // colMapper maps column to its index
	transitiveMatrix [][]bool       // transitiveMatrix[i][j] = true means we can infer that col i = col j
	eqList           []*Constant    // if eqList[i] != nil, it means col i = eqList[i]
	columns          []*Column      // columns stores all columns appearing in the conditions
	conditions       []Expression
	foreverFalse     bool
}

// propagateInEQ propagates all in-equal conditions.
// e.g. For expression a = b and b = c and c = d and c < 1 , we can get a < 1 and b < 1.
// We maintain a matrix representing the equivalent for every two columns.
func (s *propagateConstantSolver) propagateInEQ() {
	s.transitiveMatrix = make([][]bool, len(s.columns))
	for i := range s.transitiveMatrix {
		s.transitiveMatrix[i] = make([]bool, len(s.columns))
	}
	for i := 0; i < len(s.conditions); i++ {
		if fun, ok := s.conditions[i].(*ScalarFunction); ok && fun.FuncName.L == ast.EQ {
			lCol, lOk := fun.Args[0].(*Column)
			rCol, rOk := fun.Args[1].(*Column)
			if lOk && rOk {
				lID := s.getColID(lCol)
				rID := s.getColID(rCol)
				s.transitiveMatrix[lID][rID] = true
				s.transitiveMatrix[rID][lID] = true
			}
		}
	}
	colLen := len(s.colMapper)
	// We implement a floyd-warshall algorithm, see https://en.wikipedia.org/wiki/Floyd%E2%80%93Warshall_algorithm.
	for k := 0; k < colLen; k++ {
		for i := 0; i < colLen; i++ {
			for j := 0; j < colLen; j++ {
				if !s.transitiveMatrix[i][j] {
					s.transitiveMatrix[i][j] = s.transitiveMatrix[i][k] && s.transitiveMatrix[k][j]
				}
			}
		}
	}
	condsLen := len(s.conditions)
	for i := 0; i < condsLen; i++ {
		cond := s.conditions[i]
		col, con := s.validPropagateCond(cond, inEqFuncNameMap)
		if col != nil {
			id := s.getColID(col)
			for to, connected := range s.transitiveMatrix[id] {
				if to != id && connected {
					newFunc, _ := NewFunction(cond.(*ScalarFunction).FuncName.L, cond.GetType(), s.columns[to], con)
					s.conditions = append(s.conditions, newFunc)
				}
			}
		}
	}
}

// propagatesEQ propagates equal expression multiple times. Like a = d and b * 2 = c and c = d + 2 and b = 1, the process is:
// a = d & b * 2 = c & c = d + 2 & b = 1 & a = 4, we pick eq cond b = 1 and a = 4
// d = 4 & 2 = c & c = d + 2 & b = 1 & a = 4, we propagate b = 1 and a = 4 and pick eq cond c = 2 and d = 4
// d = 4 & 2 = c & false & b = 1 & a = 4, we propagate c = 2 and d = 4, and do constant folding: c = d + 2 will be folded as false.
func (s *propagateConstantSolver) propagateEQ() {
	s.eqList = make([]*Constant, len(s.columns))
	visited := make([]bool, len(s.conditions))
	for i := 0; i < MaxPropagateColsCnt; i++ {
		mapper := s.pickNewEQConds(visited)
		if s.foreverFalse || len(mapper) == 0 {
			return
		}
		cols := make(Schema, 0, len(mapper))
		cons := make([]Expression, 0, len(mapper))
		for id, con := range mapper {
			cols = append(cols, s.columns[id])
			cons = append(cons, con)
		}
		for i, cond := range s.conditions {
			if !visited[i] {
				s.conditions[i] = ColumnSubstitute(cond, Schema(cols), cons)
			}
		}
	}
}

// In this function we check if the cond is an expression like [column op constant] and op is in the funNameMap.
func (s *propagateConstantSolver) validPropagateCond(cond Expression, funNameMap map[string]bool) (*Column, *Constant) {
	if eq, ok := cond.(*ScalarFunction); ok {
		if _, ok := funNameMap[eq.FuncName.L]; !ok {
			return nil, nil
		}
		if col, colOk := eq.Args[0].(*Column); colOk {
			if con, conOk := eq.Args[1].(*Constant); conOk {
				return col, con
			}
		}
		if col, colOk := eq.Args[1].(*Column); colOk {
			if con, conOk := eq.Args[0].(*Constant); conOk {
				return col, con
			}
		}
	}
	return nil, nil
}

// pickNewEQConds tries to pick new equal conds and puts them to retMapper.
func (s *propagateConstantSolver) pickNewEQConds(visited []bool) (retMapper map[int]*Constant) {
	retMapper = make(map[int]*Constant)
	for i, cond := range s.conditions {
		if !visited[i] {
			col, con := s.validPropagateCond(cond, eqFuncNameMap)
			if col != nil {
				visited[i] = true
				if s.tryToUpdateEQList(col, con) {
					retMapper[s.getColID(col)] = con
				} else if s.foreverFalse {
					return
				}
			}
		}
	}
	return
}

// tryToUpdateEQList tries to update the eqList. When the eqList has store this column with a different constant, like
// a = 1 and a = 2, we set conditions to false.
func (s *propagateConstantSolver) tryToUpdateEQList(col *Column, con *Constant) bool {
	if con.Value.IsNull() {
		s.foreverFalse = true
		s.conditions = []Expression{&Constant{
			Value:   types.NewDatum(false),
			RetType: types.NewFieldType(mysql.TypeTiny),
		}}
		return false
	}
	id := s.getColID(col)
	oldCon := s.eqList[id]
	if oldCon != nil {
		log.Warnf("old %s new %s", oldCon, con)
		if !oldCon.Equal(con) {
			s.foreverFalse = true
			s.conditions = []Expression{&Constant{
				Value:   types.NewDatum(false),
				RetType: types.NewFieldType(mysql.TypeTiny),
			}}
		}
		return false
	}
	s.eqList[id] = con
	return true
}

func (s *propagateConstantSolver) solve(conditions []Expression) []Expression {
	var cols []*Column
	for _, cond := range conditions {
		s.conditions = append(s.conditions, SplitCNFItems(cond)...)
		cols = append(cols, ExtractColumns(cond)...)
	}
	for _, col := range cols {
		s.insertCol(col)
	}
	if len(s.columns) > MaxPropagateColsCnt {
		log.Warnf("[const_propagation]Too many columns in a single CNF: the column count is %d, the max count is %d.", len(s.columns), MaxPropagateColsCnt)
		return conditions
	}
	s.propagateEQ()
	s.propagateInEQ()
	for i, cond := range s.conditions {
		if dnf, ok := cond.(*ScalarFunction); ok && dnf.FuncName.L == ast.OrOr {
			dnfItems := SplitDNFItems(cond)
			for j, item := range dnfItems {
				dnfItems[j] = ComposeCNFCondition(PropagateConstant([]Expression{item}))
			}
			s.conditions[i] = ComposeDNFCondition(dnfItems)
		}
	}
	return s.conditions
}

func (s *propagateConstantSolver) getColID(col *Column) int {
	code := col.HashCode()
	return s.colMapper[string(code)]
}

func (s *propagateConstantSolver) insertCol(col *Column) {
	code := col.HashCode()
	_, ok := s.colMapper[string(code)]
	if !ok {
		s.colMapper[string(code)] = len(s.colMapper)
		s.columns = append(s.columns, col)
	}
}

// PropagateConstant propagate constant values of equality predicates and inequality predicates in a condition.
func PropagateConstant(conditions []Expression) []Expression {
	solver := &propagateConstantSolver{
		colMapper: make(map[string]int),
	}
	return solver.solve(conditions)
}
