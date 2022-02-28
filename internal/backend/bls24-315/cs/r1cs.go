// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package cs

import (
	"errors"
	"fmt"
	"github.com/fxamacker/cbor/v2"
	"io"
	"math/big"
	"runtime"
	"strings"
	"sync"

	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/witness"
	"github.com/consensys/gnark/frontend/compiled"
	"github.com/consensys/gnark/frontend/schema"
	"github.com/consensys/gnark/internal/backend/ioutils"

	"github.com/consensys/gnark-crypto/ecc"
	"math"

	"github.com/consensys/gnark-crypto/ecc/bls24-315/fr"

	bls24_315witness "github.com/consensys/gnark/internal/backend/bls24-315/witness"
)

// R1CS decsribes a set of R1CS constraint
type R1CS struct {
	compiled.R1CS
	Coefficients []fr.Element // R1C coefficients indexes point here
}

// NewR1CS returns a new R1CS and sets cs.Coefficient (fr.Element) from provided big.Int values
func NewR1CS(cs compiled.R1CS, coefficients []big.Int) *R1CS {
	r := R1CS{
		R1CS:         cs,
		Coefficients: make([]fr.Element, len(coefficients)),
	}
	for i := 0; i < len(coefficients); i++ {
		r.Coefficients[i].SetBigInt(&coefficients[i])
	}

	return &r
}

// Solve sets all the wires and returns the a, b, c vectors.
// the cs system should have been compiled before. The entries in a, b, c are in Montgomery form.
// a, b, c vectors: ab-c = hz
// witness = [publicWires | secretWires] (without the ONE_WIRE !)
// returns  [publicWires | secretWires | internalWires ]
func (cs *R1CS) Solve(witness, a, b, c []fr.Element, opt backend.ProverConfig) ([]fr.Element, error) {

	nbWires := cs.NbPublicVariables + cs.NbSecretVariables + cs.NbInternalVariables
	solution, err := newSolution(nbWires, opt.HintFunctions, cs.Coefficients)
	if err != nil {
		return make([]fr.Element, nbWires), err
	}

	if len(witness) != int(cs.NbPublicVariables-1+cs.NbSecretVariables) { // - 1 for ONE_WIRE
		return solution.values, fmt.Errorf("invalid witness size, got %d, expected %d = %d (public - ONE_WIRE) + %d (secret)", len(witness), int(cs.NbPublicVariables-1+cs.NbSecretVariables), cs.NbPublicVariables-1, cs.NbSecretVariables)
	}

	// compute the wires and the a, b, c polynomials
	if len(a) != len(cs.Constraints) || len(b) != len(cs.Constraints) || len(c) != len(cs.Constraints) {
		return solution.values, errors.New("invalid input size: len(a, b, c) == len(Constraints)")
	}

	solution.solved[0] = true // ONE_WIRE
	solution.values[0].SetOne()
	copy(solution.values[1:], witness)
	for i := 0; i < len(witness); i++ {
		solution.solved[i+1] = true
	}

	// keep track of the number of wire instantiations we do, for a sanity check to ensure
	// we instantiated all wires
	solution.nbSolved += uint64(len(witness) + 1)

	// now that we know all inputs are set, defer log printing once all solution.values are computed
	// (or sooner, if a constraint is not satisfied)
	defer solution.printLogs(opt.LoggerOut, cs.Logs)

	if err := cs.parallelSolve(a, b, c, &solution); err != nil {
		return solution.values, err
	}

	// sanity check; ensure all wires are marked as "instantiated"
	if !solution.isValid() {
		panic("solver didn't instantiate all wires")
	}

	return solution.values, nil
}

func (cs *R1CS) parallelSolve(a, b, c []fr.Element, solution *solution) error {
	// minWorkPerCPU is the minimum target number of constraint a task should hold
	// in other words, if a level has less than minWorkPerCPU, it will not be parallelized and executed
	// sequentially without sync.
	const minWorkPerCPU = 50.0

	// cs.Levels has a list of levels, where all constraints in a level l(n) are independent
	// and may only have dependencies on previous levels
	// for each constraint
	// we are guaranteed that each R1C contains at most one unsolved wire
	// first we solve the unsolved wire (if any)
	// then we check that the constraint is valid
	// if a[i] * b[i] != c[i]; it means the constraint is not satisfied

	var wg sync.WaitGroup
	chTasks := make(chan []int, runtime.NumCPU())
	chError := make(chan error, runtime.NumCPU())

	// start a worker pool
	// each worker wait on chTasks
	// a task is a slice of constraint indexes to be solved
	for i := 0; i < runtime.NumCPU(); i++ {
		go func() {
			for t := range chTasks {
				for _, i := range t {
					// for each constraint in the task, solve it.
					if err := cs.solveConstraint(cs.Constraints[i], solution, &a[i], &b[i], &c[i]); err != nil {
						if err == errUnsatisfiedConstraint {
							if dID, ok := cs.MDebug[int(i)]; ok {
								err = errors.New(solution.logValue(cs.DebugInfo[dID]))
							} else {
								err = fmt.Errorf("%s ⋅ %s != %s", a[i].String(), b[i].String(), c[i].String())
							}
						}
						chError <- fmt.Errorf("constraint #%d is not satisfied: %w", i, err)
						wg.Done()
						return
					}
				}
				wg.Done()
			}
		}()
	}

	// clean up pool go routines
	defer func() {
		close(chTasks)
		close(chError)
	}()

	// for each level, we push the tasks
	for _, level := range cs.Levels {

		// max CPU to use
		maxCPU := float64(len(level)) / minWorkPerCPU

		if maxCPU <= 1.0 {
			// we do it sequentially
			for _, i := range level {
				if err := cs.solveConstraint(cs.Constraints[i], solution, &a[i], &b[i], &c[i]); err != nil {
					if err == errUnsatisfiedConstraint {
						if dID, ok := cs.MDebug[int(i)]; ok {
							err = errors.New(solution.logValue(cs.DebugInfo[dID]))
						} else {
							err = fmt.Errorf("%s ⋅ %s != %s", a[i].String(), b[i].String(), c[i].String())
						}
					}
					return fmt.Errorf("constraint #%d is not satisfied: %w", i, err)
				}
			}
			continue
		}

		// number of tasks for this level is set to num cpus
		// but if we don't have enough work for all our CPUS, it can be lower.
		nbTasks := runtime.NumCPU()
		maxTasks := int(math.Ceil(maxCPU))
		if nbTasks > maxTasks {
			nbTasks = maxTasks
		}
		nbIterationsPerCpus := len(level) / nbTasks

		// more CPUs than tasks: a CPU will work on exactly one iteration
		// note: this depends on minWorkPerCPU constant
		if nbIterationsPerCpus < 1 {
			nbIterationsPerCpus = 1
			nbTasks = len(level)
		}

		extraTasks := len(level) - (nbTasks * nbIterationsPerCpus)
		extraTasksOffset := 0

		for i := 0; i < nbTasks; i++ {
			wg.Add(1)
			_start := i*nbIterationsPerCpus + extraTasksOffset
			_end := _start + nbIterationsPerCpus
			if extraTasks > 0 {
				_end++
				extraTasks--
				extraTasksOffset++
			}
			// since we're never pushing more than num CPU tasks
			// we will never be blocked here
			chTasks <- level[_start:_end]
		}

		// wait for the level to be done
		wg.Wait()

		if len(chError) > 0 {
			return <-chError
		}
	}

	return nil
}

// IsSolved returns nil if given witness solves the R1CS and error otherwise
// this method wraps cs.Solve() and allocates cs.Solve() inputs
func (cs *R1CS) IsSolved(witness *witness.Witness, opts ...backend.ProverOption) error {
	opt, err := backend.NewProverConfig(opts...)
	if err != nil {
		return err
	}

	a := make([]fr.Element, len(cs.Constraints))
	b := make([]fr.Element, len(cs.Constraints))
	c := make([]fr.Element, len(cs.Constraints))
	v := witness.Vector.(*bls24_315witness.Witness)
	_, err = cs.Solve(*v, a, b, c, opt)
	return err
}

// divByCoeff sets res = res / t.Coeff
func (cs *R1CS) divByCoeff(res *fr.Element, t compiled.Term) {
	cID := t.CoeffID()
	switch cID {
	case compiled.CoeffIdOne:
		return
	case compiled.CoeffIdMinusOne:
		res.Neg(res)
	case compiled.CoeffIdZero:
		panic("division by 0")
	default:
		// this is slow, but shouldn't happen as divByCoeff is called to
		// remove the coeff of an unsolved wire
		// but unsolved wires are (in gnark frontend) systematically set with a coeff == 1 or -1
		res.Div(res, &cs.Coefficients[cID])
	}
}

// solveConstraint compute unsolved wires in the constraint, if any and set the solution accordingly
//
// returns an error if the solver called a hint function that errored
// returns false, nil if there was no wire to solve
// returns true, nil if exactly one wire was solved. In that case, it is redundant to check that
// the constraint is satisfied later.
func (cs *R1CS) solveConstraint(r compiled.R1C, solution *solution, a, b, c *fr.Element) error {

	// the index of the non zero entry shows if L, R or O has an uninstantiated wire
	// the content is the ID of the wire non instantiated
	var loc uint8

	var termToCompute compiled.Term

	processLExp := func(l compiled.LinearExpression, val *fr.Element, locValue uint8) error {
		for _, t := range l {
			vID := t.WireID()

			// wire is already computed, we just accumulate in val
			if solution.solved[vID] {
				solution.accumulateInto(t, val)
				continue
			}

			// first we check if this is a hint wire
			if hint, ok := cs.MHints[vID]; ok {
				if err := solution.solveWithHint(vID, hint); err != nil {
					return err
				}
				// now that the wire is saved, accumulate it into a, b or c
				solution.accumulateInto(t, val)
				continue
			}

			if loc != 0 {
				panic("found more than one wire to instantiate")
			}
			termToCompute = t
			loc = locValue
		}
		return nil
	}

	if err := processLExp(r.L, a, 1); err != nil {
		return err
	}

	if err := processLExp(r.R, b, 2); err != nil {
		return err
	}

	if err := processLExp(r.O, c, 3); err != nil {
		return err
	}

	if loc == 0 {
		// there is nothing to solve, may happen if we have an assertion
		// (ie a constraints that doesn't yield any output)
		// or if we solved the unsolved wires with hint functions
		var check fr.Element
		if !check.Mul(a, b).Equal(c) {
			return errUnsatisfiedConstraint
		}
		return nil
	}

	// we compute the wire value and instantiate it
	wID := termToCompute.WireID()

	// solver result
	var wire fr.Element

	switch loc {
	case 1:
		if !b.IsZero() {
			wire.Div(c, b).
				Sub(&wire, a)
			a.Add(a, &wire)
		} else {
			// we didn't actually ensure that a * b == c
			var check fr.Element
			if !check.Mul(a, b).Equal(c) {
				return errUnsatisfiedConstraint
			}
		}
	case 2:
		if !a.IsZero() {
			wire.Div(c, a).
				Sub(&wire, b)
			b.Add(b, &wire)
		} else {
			var check fr.Element
			if !check.Mul(a, b).Equal(c) {
				return errUnsatisfiedConstraint
			}
		}
	case 3:
		wire.Mul(a, b).
			Sub(&wire, c)

		c.Add(c, &wire)
	}

	// wire is the term (coeff * value)
	// but in the solution we want to store the value only
	// note that in gnark frontend, coeff here is always 1 or -1
	cs.divByCoeff(&wire, termToCompute)
	solution.set(wID, wire)

	return nil
}

// GetConstraints return a list of constraint formatted as L⋅R == O
// such that [0] -> L, [1] -> R, [2] -> O
func (cs *R1CS) GetConstraints() [][]string {
	r := make([][]string, 0, len(cs.Constraints))
	for _, c := range cs.Constraints {
		// for each constraint, we build a string representation of it's L, R and O part
		// if we are worried about perf for large cs, we could do a string builder + csv format.
		var line [3]string
		line[0] = cs.vtoString(c.L)
		line[1] = cs.vtoString(c.R)
		line[2] = cs.vtoString(c.O)
		r = append(r, line[:])
	}
	return r
}

func (cs *R1CS) vtoString(l compiled.LinearExpression) string {
	var sbb strings.Builder
	for i := 0; i < len(l); i++ {
		cs.termToString(l[i], &sbb)
		if i+1 < len(l) {
			sbb.WriteString(" + ")
		}
	}
	return sbb.String()
}

func (cs *R1CS) termToString(t compiled.Term, sbb *strings.Builder) {
	tID := t.CoeffID()
	if tID == compiled.CoeffIdOne {
		// do nothing, just print the variable
	} else if tID == compiled.CoeffIdMinusOne {
		// print neg sign
		sbb.WriteByte('-')
	} else if tID == compiled.CoeffIdZero {
		sbb.WriteByte('0')
		return
	} else {
		sbb.WriteString(cs.Coefficients[tID].String())
		sbb.WriteString("⋅")
	}
	vID := t.WireID()
	visibility := t.VariableVisibility()

	switch visibility {
	case schema.Internal:
		if _, isHint := cs.MHints[vID]; isHint {
			sbb.WriteString(fmt.Sprintf("hv%d", vID-cs.NbPublicVariables-cs.NbSecretVariables))
		} else {
			sbb.WriteString(fmt.Sprintf("v%d", vID-cs.NbPublicVariables-cs.NbSecretVariables))
		}
	case schema.Public:
		if vID == 0 {
			sbb.WriteByte('1') // one wire
		} else {
			sbb.WriteString(fmt.Sprintf("p%d", vID-1))
		}
	case schema.Secret:
		sbb.WriteString(fmt.Sprintf("s%d", vID-cs.NbPublicVariables))
	default:
		sbb.WriteString("<?>")
	}
}

// GetNbCoefficients return the number of unique coefficients needed in the R1CS
func (cs *R1CS) GetNbCoefficients() int {
	return len(cs.Coefficients)
}

// CurveID returns curve ID as defined in gnark-crypto
func (cs *R1CS) CurveID() ecc.ID {
	return ecc.BLS24_315
}

// FrSize return fr.Limbs * 8, size in byte of a fr element
func (cs *R1CS) FrSize() int {
	return fr.Limbs * 8
}

// WriteTo encodes R1CS into provided io.Writer using cbor
func (cs *R1CS) WriteTo(w io.Writer) (int64, error) {
	_w := ioutils.WriterCounter{W: w} // wraps writer to count the bytes written
	enc, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		return 0, err
	}
	encoder := enc.NewEncoder(&_w)

	// encode our object
	err = encoder.Encode(cs)
	return _w.N, err
}

// ReadFrom attempts to decode R1CS from io.Reader using cbor
func (cs *R1CS) ReadFrom(r io.Reader) (int64, error) {
	dm, err := cbor.DecOptions{
		MaxArrayElements: 134217728,
		MaxMapPairs:      134217728,
	}.DecMode()

	if err != nil {
		return 0, err
	}
	decoder := dm.NewDecoder(r)
	if err := decoder.Decode(&cs); err != nil {
		return int64(decoder.NumBytesRead()), err
	}

	return int64(decoder.NumBytesRead()), nil
}
