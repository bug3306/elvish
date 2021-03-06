// Package eval handles evaluation of nodes and consists the runtime of the
// shell.
package eval

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/elves/elvish/errutil"
	"github.com/elves/elvish/parse"
	"github.com/elves/elvish/store"
)

// ns is a namespace.
type ns map[string]Variable

// Evaluator maintains runtime context of elvish code within a single
// goroutine. When elvish code spawns goroutines, the Evaluator is copied and
// has certain components replaced.
type Evaluator struct {
	Compiler *Compiler
	evaluatorEphemeral
	local       ns
	up          ns
	builtin     ns
	mod         map[string]ns
	searchPaths []string
	ports       []*port
	statusCb    func([]Value)
	store       *store.Store
}

// evaluatorEphemeral holds the ephemeral parts of an Evaluator, namely the
// parts only valid through one startEval-stopEval cycle.
type evaluatorEphemeral struct {
	name, text, context string
}

func statusOk(vs []Value) bool {
	for _, v := range vs {
		v, ok := v.(exitus)
		if !ok {
			return false
		}
		if !v.Success {
			return false
		}
	}
	return true
}

// NewEvaluator creates a new top-level Evaluator.
func NewEvaluator(st *store.Store, dataDir string) *Evaluator {
	pid := str(strconv.Itoa(syscall.Getpid()))
	bi := ns{
		"pid":     newInternalVariableWithType(pid),
		"success": newInternalVariableWithType(success),
		"true":    newInternalVariableWithType(boolean(true)),
		"false":   newInternalVariableWithType(boolean(false)),
	}
	for _, b := range builtinFns {
		bi["fn-"+b.Name] = newInternalVariableWithType(b)
	}
	ev := &Evaluator{
		Compiler: NewCompiler(makeCompilerScope(bi), dataDir),
		local:    ns{},
		up:       ns{},
		builtin:  bi,
		mod:      map[string]ns{},
		ports: []*port{
			&port{f: os.Stdin}, &port{f: os.Stdout}, &port{f: os.Stderr}},
		statusCb: func(vs []Value) {
			if statusOk(vs) {
				return
			}
			fmt.Print("Status: ")
			for i, v := range vs {
				if i > 0 {
					fmt.Print(" ")
				}
				fmt.Print(v.Repr())
			}
			fmt.Println()
		},
		store: st,
	}
	path := os.Getenv("PATH")
	if path != "" {
		ev.searchPaths = strings.Split(path, ":")
		// fmt.Printf("Search paths are %v\n", search_paths)
	} else {
		ev.searchPaths = []string{"/bin"}
	}

	return ev
}

// SetChanOut sets the channel output.
func (ev *Evaluator) SetChanOut(ch chan Value) {
	ev.ports[1].ch = ch
}

// copy returns a copy of ev with context changed. ev.ports is copied deeply
// and all shouldClose flags are reset.
//
// NOTE(xiaq): Subevaluators are relied upon for calling closePorts.
func (ev *Evaluator) copy(context string) *Evaluator {
	newEv := new(Evaluator)
	*newEv = *ev
	newEv.context = context
	// Do a deep copy of ports and reset shouldClose flags
	newEv.ports = make([]*port, len(ev.ports))
	for i, p := range ev.ports {
		newEv.ports[i] = &port{p.f, p.ch, false, false}
	}
	return newEv
}

// port returns ev.ports[i] or nil if i is out of range. This makes it possible
// to treat ev.ports as if it has an infinite tail of nil's.
func (ev *Evaluator) port(i int) *port {
	if i >= len(ev.ports) {
		return nil
	}
	return ev.ports[i]
}

// growPorts makes the size of ev.ports at least n, adding nil's if necessary.
func (ev *Evaluator) growPorts(n int) {
	if len(ev.ports) >= n {
		return
	}
	ports := ev.ports
	ev.ports = make([]*port, n)
	copy(ev.ports, ports)
}

// makeCompilerScope extracts the type information from variables.
func makeCompilerScope(s ns) staticNS {
	scope := staticNS{}
	for name, variable := range s {
		scope[name] = variable.StaticType()
	}
	return scope
}

// Eval evaluates a chunk node n. The supplied name and text are used in
// diagnostic messages.
func (ev *Evaluator) Eval(name, text, dir string, n *parse.ChunkNode) error {
	op, err := ev.Compiler.Compile(name, text, dir, n)
	if err != nil {
		return err
	}
	return ev.eval(name, text, op)
}

// eval evaluates an Op.
func (ev *Evaluator) eval(name, text string, op Op) (err error) {
	if op == nil {
		return nil
	}
	ev.startEval(name, text)
	defer ev.stopEval()
	defer errutil.Catch(&err)
	op(ev)
	return nil
}

func (ev *Evaluator) startEval(name, text string) {
	ev.evaluatorEphemeral = evaluatorEphemeral{name, text, "top"}
}

func (ev *Evaluator) stopEval() {
	ev.evaluatorEphemeral = evaluatorEphemeral{}
}

// errorf stops the ev.eval immediately by panicking with a diagnostic message.
// The panic is supposed to be caught by ev.eval.
func (ev *Evaluator) errorf(p parse.Pos, format string, args ...interface{}) {
	errutil.Throw(errutil.NewContextualError(
		fmt.Sprintf("%s (%s)", ev.name, ev.context), "evalling error",
		ev.text, int(p), format, args...))
}

// mustSingleString returns a String if that is the only element of vs.
// Otherwise it errors.
func (ev *Evaluator) mustSingleString(vs []Value, what string, p parse.Pos) str {
	if len(vs) != 1 {
		ev.errorf(p, "Expect exactly one word for %s, got %d", what, len(vs))
	}
	v, ok := vs[0].(str)
	if !ok {
		ev.errorf(p, "Expect string for %s, got %s", what, vs[0])
	}
	return v
}

func (ev *Evaluator) applyPortOps(ports []portOp) {
	ev.growPorts(len(ports))

	for i, op := range ports {
		if op != nil {
			ev.ports[i] = op(ev)
		}
	}
}

func (ev *Evaluator) SourceText(name, src, dir string) error {
	n, err := parse.Parse(name, src)
	if err != nil {
		return err
	}
	return ev.Eval(name, src, dir, n)
}

func readFileUTF8(fname string) (string, error) {
	bytes, err := ioutil.ReadFile(fname)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(bytes) {
		return "", fmt.Errorf("%s: source is not valid UTF-8", fname)
	}
	return string(bytes), nil
}

// Source evaluates the content of a file.
func (ev *Evaluator) Source(fname string) error {
	src, err := readFileUTF8(fname)
	if err != nil {
		return err
	}
	return ev.SourceText(fname, src, path.Dir(fname))
}

// ResolveVar resolves a variable. When the variable cannot be found, nil is
// returned.
func (ev *Evaluator) ResolveVar(ns, name string) Variable {
	if ns == "env" {
		return newEnvVariable(name)
	}
	if mod, ok := ev.mod[ns]; ok {
		return mod[name]
	}

	may := func(n string) bool {
		return ns == "" || ns == n
	}
	if may("local") {
		if v, ok := ev.local[name]; ok {
			return v
		}
	}
	if may("up") {
		if v, ok := ev.up[name]; ok {
			return v
		}
	}
	if may("builtin") {
		if v, ok := ev.builtin[name]; ok {
			return v
		}
	}
	return nil
}
