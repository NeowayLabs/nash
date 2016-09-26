package sh

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/NeowayLabs/nash/ast"
	"github.com/NeowayLabs/nash/errors"
	"github.com/NeowayLabs/nash/parser"
	"github.com/NeowayLabs/nash/token"
)

const (
	logNS     = "nash.Shell"
	defPrompt = "\033[31mλ>\033[0m "
)

type (
	// Env is the environment map of lists
	Env map[string]*Obj
	Var Env
	Fns map[string]Fn

	StatusCode uint8

	Runner interface {
		Start() error
		Wait() error
		Results() *Obj

		SetArgs([]ast.Expr, *Shell) error
		SetEnviron([]string)
		SetStdin(io.Reader)
		SetStdout(io.Writer)
		SetStderr(io.Writer)

		StdoutPipe() (io.ReadCloser, error)

		Stdin() io.Reader
		Stdout() io.Writer
		Stderr() io.Writer
	}

	Fn interface {
		Name() string
		ArgNames() []string

		Runner

		String() string
	}

	// Shell is the core data structure.
	Shell struct {
		name        string
		debug       bool
		lambdas     uint
		logf        LogFn
		nashdPath   string
		isFn        bool
		currentFile string // current file being executed or imported

		interrupted bool
		looping     bool

		stdin  io.Reader
		stdout io.Writer
		stderr io.Writer

		env  Env
		vars Var
		fns  Fns

		builtins Fns
		binds    Fns

		root   *ast.Tree
		parent *Shell

		repr string // string representation

		*sync.Mutex
	}

	errIgnore struct {
		*errors.NashError
	}

	errInterrupted struct {
		*errors.NashError
	}

	errStopWalking struct {
		*errors.NashError
	}
)

const (
	ESuccess    StatusCode = 0
	ENotFound              = 127
	ENotStarted            = 255
)

func newErrIgnore(format string, arg ...interface{}) error {
	e := &errIgnore{
		NashError: errors.NewError(format, arg...),
	}

	return e
}

func (e *errIgnore) Ignore() bool { return true }

func newErrInterrupted(format string, arg ...interface{}) error {
	return &errInterrupted{
		NashError: errors.NewError(format, arg...),
	}
}

func (e *errInterrupted) Interrupted() bool { return true }

func newErrStopWalking() *errStopWalking {
	return &errStopWalking{
		NashError: errors.NewError("return"),
	}
}

func (e *errStopWalking) StopWalking() bool { return true }

// NewShell creates a new shell object
func NewShell() (*Shell, error) {
	sh := &Shell{
		name:      "parent scope",
		isFn:      false,
		logf:      NewLog(logNS, false),
		nashdPath: nashdAutoDiscover(),
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		stdin:     os.Stdin,
		env:       make(Env),
		vars:      make(Var),
		fns:       make(Fns),
		builtins:  make(Fns),
		binds:     make(Fns),
		Mutex:     &sync.Mutex{},
	}

	err := sh.setup()

	if err != nil {
		return nil, err
	}

	sh.setupSignals()

	return sh, nil
}

// NewSubShell creates a nash.Shell that inherits the parent shell stdin,
// stdout, stderr and mutex lock.
// Every variable and function lookup is done first in the subshell and then, if
// not found, in the parent shell recursively.
func NewSubShell(name string, parent *Shell) (*Shell, error) {
	if parent == nil {
		return nil, errors.NewError("A sub Shell requires a parent shell")
	}

	sh := &Shell{
		name:      name,
		isFn:      true,
		parent:    parent,
		logf:      NewLog(logNS, false),
		nashdPath: nashdAutoDiscover(),
		stdout:    parent.Stdout(),
		stderr:    parent.Stderr(),
		stdin:     parent.Stdin(),
		env:       make(Env),
		vars:      make(Var),
		fns:       make(Fns),
		binds:     make(Fns),
		builtins:  nil, // subshell does not have builtins
		Mutex:     parent.Mutex,
	}

	return sh, nil
}

// initEnv creates a new environment from old one
func (sh *Shell) initEnv(processEnv []string) error {
	largs := make([]*Obj, len(os.Args))

	for i := 0; i < len(os.Args); i++ {
		largs[i] = NewStrObj(os.Args[i])
	}

	argv := NewListObj(largs)

	sh.Setenv("argv", argv)
	sh.Setvar("argv", argv)

	for _, penv := range processEnv {
		var value *Obj
		p := strings.Split(penv, "=")

		if len(p) == 2 {
			// TODO(i4k): handle lists correctly in the future
			// argv is not special, every list must be handled correctly
			if p[0] == "argv" {
				continue
			}

			value = NewStrObj(p[1])

			sh.Setvar(p[0], value)
			sh.Setenv(p[0], value)
		}
	}

	pidVal := NewStrObj(strconv.Itoa(os.Getpid()))

	sh.Setenv("PID", pidVal)
	sh.Setvar("PID", pidVal)

	if _, ok := sh.Getenv("SHELL"); !ok {
		shellVal := NewStrObj(nashdAutoDiscover())
		sh.Setenv("SHELL", shellVal)
		sh.Setvar("SHELL", shellVal)
	}

	cwd, err := os.Getwd()

	if err != nil {
		return err
	}

	cwdObj := NewStrObj(cwd)
	sh.Setenv("PWD", cwdObj)
	sh.Setvar("PWD", cwdObj)

	return nil
}

// Reset internal state
func (sh *Shell) Reset() {
	sh.fns = make(Fns)
	sh.vars = make(Var)
	sh.env = make(Env)
	sh.binds = make(Fns)
}

// SetDebug enable/disable debug in the shell
func (sh *Shell) SetDebug(d bool) {
	sh.debug = d
	sh.logf = NewLog(logNS, d)
}

func (sh *Shell) SetName(a string) {
	sh.name = a
}

func (sh *Shell) Name() string { return sh.name }

func (sh *Shell) SetParent(a *Shell) {
	sh.parent = a
}

func (sh *Shell) Environ() Env {
	if sh.parent != nil {
		return sh.parent.Environ()
	}

	return sh.env
}

func (sh *Shell) Getenv(name string) (*Obj, bool) {
	if sh.parent != nil {
		return sh.parent.Getenv(name)
	}

	value, ok := sh.env[name]
	return value, ok
}

func (sh *Shell) Setenv(name string, value *Obj) {
	if sh.parent != nil {
		sh.parent.Setenv(name, value)
		return
	}

	sh.Setvar(name, value)

	sh.env[name] = value
	os.Setenv(name, value.String())
}

func (sh *Shell) SetEnviron(processEnv []string) {
	sh.env = make(Env)

	for _, penv := range processEnv {
		var value *Obj
		p := strings.Split(penv, "=")

		if len(p) == 2 {
			value = NewStrObj(p[1])

			sh.Setvar(p[0], value)
			sh.Setenv(p[0], value)
		}
	}
}

func (sh *Shell) GetVar(name string) (*Obj, bool) {
	if value, ok := sh.vars[name]; ok {
		return value, ok
	}

	if sh.parent != nil {
		return sh.parent.GetVar(name)
	}

	return nil, false
}

func (sh *Shell) GetBuiltin(name string) (Fn, bool) {
	sh.logf("Looking for builtin '%s' on shell '%s'\n", name, sh.name)

	if sh.parent != nil {
		return sh.parent.GetBuiltin(name)
	}

	if fn, ok := sh.builtins[name]; ok {
		return fn, true
	}

	return nil, false
}

func (sh *Shell) GetFn(name string) (Fn, bool) {
	sh.logf("Looking for function '%s' on shell '%s'\n", name, sh.name)

	if fn, ok := sh.fns[name]; ok {
		return fn, ok
	}

	if sh.parent != nil {
		return sh.parent.GetFn(name)
	}

	return nil, false
}

func (sh *Shell) Setbindfn(name string, value Fn) {
	sh.binds[name] = value
}

func (sh *Shell) Getbindfn(cmdName string) (Fn, bool) {
	if fn, ok := sh.binds[cmdName]; ok {
		return fn, true
	}

	if sh.parent != nil {
		return sh.parent.Getbindfn(cmdName)
	}

	return nil, false
}

func (sh *Shell) Setvar(name string, value *Obj) {
	sh.vars[name] = value
}

func (sh *Shell) IsFn() bool { return sh.isFn }

func (sh *Shell) SetIsFn(b bool) { sh.isFn = b }

// SetNashdPath sets an alternativa path to nashd
func (sh *Shell) SetNashdPath(path string) {
	sh.nashdPath = path
}

// SetStdin sets the stdin for commands
func (sh *Shell) SetStdin(in io.Reader) { sh.stdin = in }

// SetStdout sets stdout for commands
func (sh *Shell) SetStdout(out io.Writer) { sh.stdout = out }

// SetStderr sets stderr for commands
func (sh *Shell) SetStderr(err io.Writer) { sh.stderr = err }

func (sh *Shell) Stdout() io.Writer { return sh.stdout }
func (sh *Shell) Stderr() io.Writer { return sh.stderr }
func (sh *Shell) Stdin() io.Reader  { return sh.stdin }

// SetTree sets the internal tree of the interpreter. It's used for
// sub-shells like `fn`.
func (sh *Shell) SetTree(t *ast.Tree) {
	sh.root = t
}

// Tree returns the internal tree of the subshell.
func (sh *Shell) Tree() *ast.Tree { return sh.root }

// SetRepr set the string representation of the shell
func (sh *Shell) SetRepr(a string) {
	sh.repr = a
}

func (sh *Shell) String() string {
	if sh.repr != "" {
		return sh.repr
	}

	var out bytes.Buffer

	sh.dump(&out)

	return string(out.Bytes())
}

func (sh *Shell) setupBuiltin() error {
	lenfn := NewLenFn(sh)
	sh.builtins["len"] = lenfn
	sh.Setvar("len", NewFnObj(lenfn))

	appendfn := NewAppendFn(sh)
	sh.builtins["append"] = appendfn
	sh.Setvar("append", NewFnObj(appendfn))

	chdir := NewChdir(sh)
	sh.builtins["chdir"] = chdir
	sh.Setvar("chdir", NewFnObj(chdir))

	// only one builtin fn... no need for advanced machinery yet
	err := sh.Exec(sh.name, `fn nash_builtin_cd(path) {
            if $path == "" {
                    path = $HOME
            }

            chdir($path)
        }

        bindfn nash_builtin_cd cd`)

	return err
}

func (sh *Shell) setup() error {
	err := sh.initEnv(os.Environ())

	if err != nil {
		return err
	}

	if sh.env["PROMPT"] == nil {
		pobj := NewStrObj(defPrompt)
		sh.Setenv("PROMPT", pobj)
		sh.Setvar("PROMPT", pobj)
	}

	return sh.setupBuiltin()
}

func (sh *Shell) setupSignals() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGCHLD)

	go func() {
		for {
			sig := <-sigs

			switch sig {
			case syscall.SIGINT:
				sh.Lock()

				// TODO(i4k): Review implementation when interrupted inside
				// function loops
				if sh.looping {
					sh.setIntr(true)
				}

				sh.Unlock()
			case syscall.SIGCHLD:
				// dont need reaping because we dont have job control yet
				// every command is wait'ed.
			default:
				fmt.Printf("%s\n", sig)
			}
		}
	}()
}

func (sh *Shell) TriggerCTRLC() error {
	p, err := os.FindProcess(os.Getpid())

	if err != nil {
		return err
	}

	return p.Signal(syscall.SIGINT)
}

// setIntr *do not lock*. You must do it yourself!
func (sh *Shell) setIntr(b bool) {
	if sh.parent != nil {
		sh.parent.setIntr(b)
		return
	}

	sh.interrupted = b
}

// getIntr returns true if nash was interrupted by CTRL-C
func (sh *Shell) getIntr() bool {
	if sh.parent != nil {
		return sh.parent.getIntr()
	}

	return sh.interrupted
}

// Exec executes the commands specified by string content
func (sh *Shell) Exec(path, content string) error {
	p := parser.NewParser(path, content)

	tr, err := p.Parse()

	if err != nil {
		return err
	}

	_, err = sh.ExecuteTree(tr)
	return err
}

// Execute the nash file at given path
func (sh *Shell) ExecFile(path string) error {
	bkCurFile := sh.currentFile

	content, err := ioutil.ReadFile(path)

	if err != nil {
		return err
	}

	sh.currentFile = path

	defer func() {
		sh.currentFile = bkCurFile
	}()

	return sh.Exec(path, string(content))
}

// evalConcat reveives the AST representation of a concatenation of objects and
// returns the string representation, or error.
func (sh *Shell) evalConcat(path ast.Expr) (string, error) {
	var pathStr string

	if path.Type() != ast.NodeConcatExpr {
		return "", fmt.Errorf("Invalid node %+v", path)
	}

	concatExpr := path.(*ast.ConcatExpr)
	concat := concatExpr.List()

	for i := 0; i < len(concat); i++ {
		part := concat[i]

		switch part.Type() {

		case ast.NodeConcatExpr:
			return "", errors.NewError("Nested concat is not allowed")
		case ast.NodeVarExpr, ast.NodeIndexExpr:
			partValues, err := sh.evalVariable(part)

			if err != nil {
				return "", err
			}

			if partValues.Type() == ListType {
				return "", fmt.Errorf("Concat of list variables is not allowed: %v = %v", part, partValues)
			} else if partValues.Type() != StringType {
				return "", fmt.Errorf("Invalid concat element: %v", partValues)
			}

			pathStr += partValues.Str()
		case ast.NodeStringExpr:
			str, ok := part.(*ast.StringExpr)

			if !ok {
				return "", fmt.Errorf("Failed to eval string.")
			}

			pathStr += str.Value()
		case ast.NodeListExpr:
			return "", errors.NewError("Concat of lists is not allowed: %+v", part.String())
		default:
			return "", fmt.Errorf("Invalid argument: %+v", part)
		}
	}

	return pathStr, nil
}

func (sh *Shell) executeNode(node ast.Node, builtin bool) (*Obj, error) {
	var (
		obj *Obj
		err error
	)

	sh.logf("Executing node: %v\n", node)

	switch node.Type() {
	case ast.NodeImport:
		err = sh.executeImport(node.(*ast.ImportNode))
	case ast.NodeComment:
		// ignore
	case ast.NodeSetenv:
		err = sh.executeSetenv(node.(*ast.SetenvNode))
	case ast.NodeAssignment:
		err = sh.executeAssignment(node.(*ast.AssignmentNode))
	case ast.NodeExecAssign:
		err = sh.executeExecAssign(node.(*ast.ExecAssignNode))
	case ast.NodeCommand:
		err = sh.executeCommand(node.(*ast.CommandNode))
	case ast.NodePipe:
		err = sh.executePipe(node.(*ast.PipeNode))
	case ast.NodeRfork:
		err = sh.executeRfork(node.(*ast.RforkNode))
	case ast.NodeIf:
		obj, err = sh.executeIf(node.(*ast.IfNode))
	case ast.NodeFnDecl:
		err = sh.executeFnDecl(node.(*ast.FnDeclNode))
	case ast.NodeFnInv:
		// invocation ignoring output
		_, err = sh.executeFnInv(node.(*ast.FnInvNode))
	case ast.NodeFor:
		err = sh.executeFor(node.(*ast.ForNode))
	case ast.NodeBindFn:
		err = sh.executeBindFn(node.(*ast.BindFnNode))
	case ast.NodeDump:
		err = sh.executeDump(node.(*ast.DumpNode))
	case ast.NodeReturn:
		if sh.IsFn() {
			obj, err = sh.executeReturn(node.(*ast.ReturnNode))
		} else {
			err = errors.NewError("Unexpected return outside of function declaration.")
		}
	default:
		// should never get here
		return nil, errors.NewError("invalid node: %v.", node.Type())
	}

	return obj, err
}

func (sh *Shell) ExecuteTree(tr *ast.Tree) (*Obj, error) {
	return sh.executeTree(tr, true)
}

// executeTree evaluates the given tree
func (sh *Shell) executeTree(tr *ast.Tree, stopable bool) (*Obj, error) {
	if tr == nil || tr.Root == nil {
		return nil, errors.NewError("empty abstract syntax tree to execute")
	}

	root := tr.Root

	for _, node := range root.Nodes {
		obj, err := sh.executeNode(node, false)

		if err != nil {
			type (
				IgnoreError interface {
					Ignore() bool
				}

				InterruptedError interface {
					Interrupted() bool
				}

				StopWalkingError interface {
					StopWalking() bool
				}
			)

			if errIgnore, ok := err.(IgnoreError); ok && errIgnore.Ignore() {
				continue
			}

			if errInterrupted, ok := err.(InterruptedError); ok && errInterrupted.Interrupted() {
				return nil, err
			}

			if errStopWalking, ok := err.(StopWalkingError); stopable && ok && errStopWalking.StopWalking() {
				return obj, nil
			}

			return obj, err
		}
	}

	return nil, nil
}

func (sh *Shell) executeReturn(n *ast.ReturnNode) (*Obj, error) {
	if n.Return() == nil {
		return nil, newErrStopWalking()
	}

	obj, err := sh.evalExpr(n.Return())

	if err != nil {
		return nil, err
	}

	return obj, newErrStopWalking()
}

func (sh *Shell) executeImport(node *ast.ImportNode) error {
	arg := node.Path()

	obj, err := sh.evalExpr(arg)

	if err != nil {
		return err
	}

	if obj.Type() != StringType {
		return errors.NewError("Invalid type on import argument: %s", obj.Type())
	}

	fname := obj.Str()

	sh.logf("Importing '%s'", fname)

	if len(fname) > 0 && fname[0] == '/' {
		return sh.ExecFile(fname)
	}

	tries := make([]string, 0, 5)
	tries = append(tries, fname)

	var hasExt bool

	if len(fname) > 3 && fname[len(fname)-3:] == ".sh" {
		hasExt = true
	} else {
		tries = append(tries, fname+".sh")
	}

	if sh.currentFile != "" {
		tries = append(tries, path.Dir(sh.currentFile)+"/"+fname)

		if !hasExt {
			tries = append(tries, path.Dir(sh.currentFile)+"/"+fname+".sh")
		}
	}

	nashPath, ok := sh.Getenv("NASHPATH")

	if !ok {
		return errors.NewError("NASHPATH environment variable not set on shell %s", sh.name)
	} else if nashPath.Type() != StringType {
		return errors.NewError("NASHPATH must be n string")
	}

	dotDir := nashPath.String()

	tries = append(tries, dotDir+"/lib/"+fname)

	if !hasExt {
		tries = append(tries, dotDir+"/lib/"+fname+".sh")
	}

	sh.logf("Trying %q\n", tries)

	for _, path := range tries {
		d, err := os.Stat(path)

		if err != nil {
			continue
		}

		if m := d.Mode(); !m.IsDir() {
			return sh.ExecFile(path)
		}
	}

	return errors.NewError("Failed to import path '%s'. The locations below have been tried:\n \"%s\"",
		fname,
		strings.Join(tries, `", "`))
}

// executePipe executes a pipe of ast.Command's. Each command can be
// a path command in the operating system or a function bind to a
// command name.
// The error of each command can be suppressed prepending it with '-' (dash).
// The error returned will be a string representing the errors (or none) of
// each command separated by '|'. The $status of pipe execution will be
// the $status of each command separated by '|'.
func (sh *Shell) executePipe(pipe *ast.PipeNode) error {
	var (
		closeFiles     []io.Closer
		closeAfterWait []io.Closer
		errIndex       int
		err            error
	)

	defer func() {
		for _, c := range closeAfterWait {
			c.Close()
		}
	}()

	nodeCommands := pipe.Commands()

	if len(nodeCommands) < 2 {
		return errors.NewError("Pipe requires at least two commands.")
	}

	cmds := make([]Runner, len(nodeCommands))
	errs := make([]string, len(nodeCommands))
	igns := make([]bool, len(nodeCommands)) // ignoreErrors
	cods := make([]string, len(nodeCommands))

	for i := 0; i < len(nodeCommands); i++ {
		errs[i] = "not started"
		cods[i] = strconv.Itoa(ENotStarted)
	}

	last := len(nodeCommands) - 1

	envVars := buildenv(sh.Environ())

	// Create all commands
	for i := 0; i < len(nodeCommands); i++ {
		var (
			cmd    Runner
			ignore bool
		)

		nodeCmd := nodeCommands[i]

		cmd, ignore, err = sh.getCommand(nodeCmd)

		igns[i] = ignore

		if err != nil {
			errIndex = i
			cods[i] = strconv.Itoa(ENotFound)
			goto pipeError
		}

		// SetEnviron must be called before SetArgs
		// otherwise the subshell will have the arguments
		// shadowed by parent env
		cmd.SetEnviron(envVars)
		err = cmd.SetArgs(nodeCmd.Args(), sh)

		if err != nil {
			errIndex = i
			goto pipeError
		}

		cmd.SetStdin(sh.stdin)
		cmd.SetStderr(sh.stderr)

		if i < last {
			closeFiles, err = sh.setRedirects(cmd, nodeCmd.Redirects())
			closeAfterWait = append(closeAfterWait, closeFiles...)

			if err != nil {
				errIndex = i
				goto pipeError
			}
		}

		cmds[i] = cmd
	}

	// Shell does not support stdin redirection yet
	cmds[0].SetStdin(sh.stdin)

	// Setup the commands. Pointing the stdin of next command to stdout of previous.
	// Except the stdout of last one
	for i, cmd := range cmds[:last] {
		var (
			stdin io.ReadCloser
		)

		cmd.SetStderr(sh.stderr)

		stdin, err = cmd.StdoutPipe()

		if err != nil {
			errIndex = i
			goto pipeError
		}

		cmds[i+1].SetStdin(stdin)
	}

	cmds[last].SetStdout(sh.stdout)
	cmds[last].SetStderr(sh.stderr)

	closeFiles, err = sh.setRedirects(cmds[last], nodeCommands[last].Redirects())
	closeAfterWait = append(closeAfterWait, closeFiles...)

	if err != nil {
		errIndex = last
		goto pipeError
	}

	for i := 0; i < len(cmds); i++ {
		cmd := cmds[i]

		err = cmd.Start()

		if err != nil {
			errIndex = i
			goto pipeError
		}

		errs[i] = "success"
		cods[i] = "0"
	}

	for i, cmd := range cmds {
		err = cmd.Wait()

		if err != nil {
			errIndex = i
			goto pipeError
		}

		errs[i] = "success"
		cods[i] = "0"
	}

	sh.Setvar("status", NewStrObj("0"))
	return nil

pipeError:
	if igns[errIndex] {
		errs[errIndex] = "none"
	} else {
		errs[errIndex] = err.Error()
	}

	cods[errIndex] = getErrStatus(err, cods[errIndex])

	err = errors.NewError(strings.Join(errs, "|"))

	// verify if all status codes are the same
	uniqCodes := make(map[string]struct{})
	var uniqCode string

	for i := 0; i < len(cods); i++ {
		uniqCodes[cods[i]] = struct{}{}
		uniqCode = cods[i]
	}

	if len(uniqCodes) == 1 {
		// if all status are the same
		sh.Setvar("status", NewStrObj(uniqCode))
	} else {
		sh.Setvar("status", NewStrObj(strings.Join(cods, "|")))
	}

	if igns[errIndex] {
		return nil
	}

	return err
}

func (sh *Shell) openRedirectLocation(location ast.Expr) (io.WriteCloser, error) {
	var (
		protocol string
	)

	locationObj, err := sh.evalExpr(location)

	if err != nil {
		return nil, err
	}

	if locationObj.Type() != StringType {
		return nil, errors.NewError("Redirection to invalid object type: %v (%s)", locationObj, locationObj.Type())
	}

	locationStr := locationObj.Str()

	if len(locationStr) > 6 {
		if locationStr[0:6] == "tcp://" {
			protocol = "tcp"
		} else if locationStr[0:6] == "udp://" {
			protocol = "udp"
		} else if len(locationStr) > 7 && locationStr[0:7] == "unix://" {
			protocol = "unix"
		}
	}

	if protocol == "" {
		return os.OpenFile(locationStr, os.O_RDWR|os.O_CREATE, 0644)
	}

	switch protocol {
	case "tcp", "udp":
		netParts := strings.Split(locationStr[6:], ":")

		if len(netParts) != 2 {
			return nil, errors.NewError("Invalid tcp/udp address: %s", locationStr)
		}

		url := netParts[0] + ":" + netParts[1]

		return net.Dial(protocol, url)
	case "unix":
		return net.Dial(protocol, locationStr[7:])
	}

	return nil, errors.NewError("Unexpected redirection value: %s", locationStr)
}

func (sh *Shell) setRedirects(cmd Runner, redirDecls []*ast.RedirectNode) ([]io.Closer, error) {
	var closeAfterWait []io.Closer

	for _, r := range redirDecls {
		closeFiles, err := sh.buildRedirect(cmd, r)
		closeAfterWait = append(closeAfterWait, closeFiles...)

		if err != nil {
			return closeAfterWait, err
		}
	}

	return closeAfterWait, nil
}

func (sh *Shell) buildRedirect(cmd Runner, redirDecl *ast.RedirectNode) ([]io.Closer, error) {
	var closeAfterWait []io.Closer

	if redirDecl.LeftFD() > 2 || redirDecl.LeftFD() < ast.RedirMapSupress {
		return closeAfterWait, errors.NewError("Invalid file descriptor redirection: fd=%d", redirDecl.LeftFD())
	}

	if redirDecl.RightFD() > 2 || redirDecl.RightFD() < ast.RedirMapSupress {
		return closeAfterWait, errors.NewError("Invalid file descriptor redirection: fd=%d", redirDecl.RightFD())
	}

	var err error

	// Note(i4k): We need to remove the repetitive code in some smarter way
	switch redirDecl.LeftFD() {
	case 0:
		return closeAfterWait, fmt.Errorf("Does not support stdin redirection yet")
	case 1:
		switch redirDecl.RightFD() {
		case 0:
			return closeAfterWait, errors.NewError("Invalid redirect mapping: %d -> %d", 1, 0)
		case 1: // do nothing
		case 2:
			cmd.SetStdout(cmd.Stderr())
		case ast.RedirMapNoValue:
			if redirDecl.Location() == nil {
				return closeAfterWait, errors.NewError("Missing file in redirection: >[%d] <??>", redirDecl.LeftFD())
			}

			file, err := sh.openRedirectLocation(redirDecl.Location())

			if err != nil {
				return closeAfterWait, err
			}

			cmd.SetStdout(file)
			closeAfterWait = append(closeAfterWait, file)
		case ast.RedirMapSupress:
			file, err := os.OpenFile("/dev/null", os.O_RDWR, 0644)

			if err != nil {
				return closeAfterWait, err
			}

			cmd.SetStdout(file)
		}
	case 2:
		switch redirDecl.RightFD() {
		case 0:
			return closeAfterWait, errors.NewError("Invalid redirect mapping: %d -> %d", 2, 1)
		case 1:
			cmd.SetStderr(cmd.Stdout())
		case 2: // do nothing
		case ast.RedirMapNoValue:
			if redirDecl.Location() == nil {
				return closeAfterWait, errors.NewError("Missing file in redirection: >[%d] <??>", redirDecl.LeftFD())
			}

			file, err := sh.openRedirectLocation(redirDecl.Location())

			if err != nil {
				return closeAfterWait, err
			}

			cmd.SetStderr(file)
			closeAfterWait = append(closeAfterWait, file)
		case ast.RedirMapSupress:
			file, err := os.OpenFile("/dev/null", os.O_RDWR, 0644)

			if err != nil {
				return closeAfterWait, err
			}

			cmd.SetStderr(file)
		}
	case ast.RedirMapNoValue:
		if redirDecl.Location() == nil {
			return closeAfterWait, errors.NewError("Missing file in redirection: >[%d] <??>", redirDecl.LeftFD())
		}

		file, err := sh.openRedirectLocation(redirDecl.Location())

		if err != nil {
			return closeAfterWait, err
		}

		cmd.SetStdout(file)
		closeAfterWait = append(closeAfterWait, file)
	}

	return closeAfterWait, err
}

func (sh *Shell) getCommand(c *ast.CommandNode) (Runner, bool, error) {
	var (
		ignoreError bool
		cmd         Runner
		err         error
	)

	cmdName := c.Name()

	sh.logf("Executing: %s\n", c.Name())

	if len(cmdName) > 1 && cmdName[0] == '-' {
		ignoreError = true
		cmdName = cmdName[1:]

		sh.logf("Ignoring error\n")
	}

	if cmdName == "" {
		return nil, false, errors.NewError("Empty command name...") // TODO: add context to error
	}

	cmd, err = NewCmd(cmdName)

	if err != nil {
		type NotFound interface {
			NotFound() bool
		}

		sh.logf("Command fails: %s", err.Error())

		if errNotFound, ok := err.(NotFound); ok && errNotFound.NotFound() {
			if fn, ok := sh.Getbindfn(cmdName); ok {
				sh.logf("Executing bind %s", cmdName)
				sh.logf("%s bind to %s", cmdName, fn)

				if len(c.Args()) > len(fn.ArgNames()) {
					err = errors.NewError("Too much arguments for"+
						" function '%s'. It expects %d args, but given %d. Arguments: %q",
						fn.Name(),
						len(fn.ArgNames()),
						len(c.Args()), c.Args())
					return nil, ignoreError, err
				}

				for i := 0 + len(c.Args()); i < len(fn.ArgNames()); i++ {
					// fill missing args with empty string
					// safe?
					c.SetArgs(append(c.Args(), ast.NewStringExpr(token.NewFileInfo(0, 0), "", true)))
				}

				return fn, ignoreError, nil
			}

			return nil, ignoreError, err
		}

		return nil, ignoreError, err
	}

	return cmd, ignoreError, nil
}

func (sh *Shell) executeCommand(c *ast.CommandNode) error {
	var (
		ignoreError    bool
		status         = "127"
		envVars        []string
		closeAfterWait []io.Closer
		cmd            Runner
		err            error
	)

	defer func() {
		for _, c := range closeAfterWait {
			c.Close()
		}
	}()

	cmd, ignoreError, err = sh.getCommand(c)

	if err != nil {
		sh.logf("FAILED here ...%s", cmd)
		goto cmdError
	}

	// SetEnviron must be called before SetArgs
	// otherwise the subshell will have the arguments
	// shadowed by parent env
	envVars = buildenv(sh.Environ())
	cmd.SetEnviron(envVars)

	err = cmd.SetArgs(c.Args(), sh)

	if err != nil {
		goto cmdError
	}

	cmd.SetStdin(sh.stdin)
	cmd.SetStdout(sh.stdout)
	cmd.SetStderr(sh.stderr)

	closeAfterWait, err = sh.setRedirects(cmd, c.Redirects())

	if err != nil {
		goto cmdError
	}

	err = cmd.Start()

	if err != nil {
		goto cmdError
	}

	err = cmd.Wait()

	if err != nil {
		goto cmdError
	}

	sh.Setvar("status", NewStrObj("0"))

	return nil

cmdError:
	sh.Setvar("status", NewStrObj(getErrStatus(err, status)))

	if ignoreError {
		return newErrIgnore(err.Error())
	}

	return err
}

func (sh *Shell) evalList(argList *ast.ListExpr) (*Obj, error) {
	values := make([]*Obj, 0, len(argList.List()))

	for _, arg := range argList.List() {
		obj, err := sh.evalExpr(arg)

		if err != nil {
			return nil, err
		}

		values = append(values, obj)
	}

	return NewListObj(values), nil
}

func (sh *Shell) evalIndexedVar(indexVar *ast.IndexExpr) (*Obj, error) {
	var (
		indexNum int
	)

	variable := indexVar.Var()
	index := indexVar.Index()

	v, err := sh.evalVariable(variable)

	if err != nil {
		return nil, err
	}

	if v.Type() != ListType {
		return nil, errors.NewError("Invalid indexing of non-list variable: %s", v.Type())
	}

	if index.Type() == ast.NodeIntExpr {
		idxArg := index.(*ast.IntExpr)
		indexNum = idxArg.Value()
	} else if index.Type() == ast.NodeVarExpr {
		idxObj, err := sh.evalVariable(index)

		if err != nil {
			return nil, err
		}

		if idxObj.Type() != StringType {
			return nil, errors.NewError("Invalid object type on index value: %s", idxObj.Type())
		}

		idxVal := idxObj.Str()
		indexNum, err = strconv.Atoi(idxVal)

		if err != nil {
			return nil, err
		}
	}

	values := v.List()

	if indexNum < 0 || indexNum >= len(values) {
		return nil, errors.NewError("Index out of bounds. len(%s) == %d, but given %d", variable.Name(), len(values), indexNum)
	}

	return values[indexNum], nil
}

func (sh *Shell) evalVariable(a ast.Expr) (*Obj, error) {
	var (
		v  *Obj
		ok bool
	)

	if a.Type() == ast.NodeIndexExpr {
		return sh.evalIndexedVar(a.(*ast.IndexExpr))
	}

	if a.Type() != ast.NodeVarExpr {
		return nil, errors.NewError("Invalid eval of non variable argument: %s", a)
	}

	vexpr := a.(*ast.VarExpr)
	varName := vexpr.Name()

	if v, ok = sh.GetVar(varName[1:]); !ok {
		return nil, fmt.Errorf("Variable %s not set on shell %s", varName, sh.name)
	}

	return v, nil
}

func (sh *Shell) evalExpr(expr ast.Expr) (*Obj, error) {
	switch expr.Type() {
	case ast.NodeStringExpr:
		str := expr.(*ast.StringExpr)
		return NewStrObj(str.Value()), nil
	case ast.NodeConcatExpr:
		concat := expr.(*ast.ConcatExpr)
		argVal, err := sh.evalConcat(concat)

		if err != nil {
			return nil, err
		}

		return NewStrObj(argVal), nil
	case ast.NodeVarExpr:
		return sh.evalVariable(expr)
	case ast.NodeIndexExpr:
		indexedVar, ok := expr.(*ast.IndexExpr)

		if !ok {
			return nil, errors.NewError("Failed to eval indexed variable")
		}

		return sh.evalIndexedVar(indexedVar)
	case ast.NodeListExpr:
		argList := expr.(*ast.ListExpr)

		return sh.evalList(argList)
	}

	return nil, errors.NewError("Invalid argument type: %+v", expr)
}

func (sh *Shell) executeSetenv(v *ast.SetenvNode) error {
	var (
		varValue *Obj
		ok       bool
	)

	varName := v.Identifier()

	if varValue, ok = sh.GetVar(varName); !ok {
		return fmt.Errorf("Variable '%s' not set on shell %s", varName, sh.name)
	}

	sh.Setenv(varName, varValue)

	return nil
}

func (sh *Shell) concatElements(expr *ast.ConcatExpr) (string, error) {
	value := ""

	list := expr.List()

	for i := 0; i < len(list); i++ {
		ec := list[i]

		obj, err := sh.evalExpr(ec)

		if err != nil {
			return "", err
		}

		if obj.Type() != StringType {
			return "", errors.NewError("Impossible to concat elements of type %s", obj.Type())
		}

		value = value + obj.String()
	}

	return value, nil
}

func (sh *Shell) executeExecAssign(v *ast.ExecAssignNode) error {
	var (
		varOut bytes.Buffer
		err    error
	)

	bkStdout := sh.stdout

	sh.SetStdout(&varOut)

	defer sh.SetStdout(bkStdout)

	assign := v.Command()

	switch assign.Type() {
	case ast.NodeCommand:
		err = sh.executeCommand(assign.(*ast.CommandNode))
	case ast.NodePipe:
		err = sh.executePipe(assign.(*ast.PipeNode))
	case ast.NodeFnInv:
		fnValues, err := sh.executeFnInv(assign.(*ast.FnInvNode))

		if err != nil {
			return err
		}

		if fnValues == nil {
			return errors.NewError("Invalid assignment from function that does not return values: %s", assign)
		}

		sh.Setvar(v.Identifier(), fnValues)
		return nil
	default:
		err = errors.NewError("Unexpected node in assignment: %s", assign.String())
	}

	var strelems []string

	outStr := string(varOut.Bytes())

	if ifs, ok := sh.GetVar("IFS"); ok && ifs.Type() == ListType && len(ifs.List()) > 0 {
		strelems = strings.FieldsFunc(outStr, func(r rune) bool {
			for _, delim := range ifs.List() {
				if delim.Type() != StringType {
					continue
				}

				if len(delim.Str()) > 0 && rune(delim.Str()[0]) == r {
					return true
				}
			}

			return false
		})

		objelems := make([]*Obj, len(strelems))

		for i := 0; i < len(strelems); i++ {
			objelems[i] = NewStrObj(strelems[i])
		}

		sh.Setvar(v.Identifier(), NewListObj(objelems))
	} else {
		sh.Setvar(v.Identifier(), NewStrObj(outStr))
	}

	return err
}

func (sh *Shell) executeAssignment(v *ast.AssignmentNode) error {
	var err error

	obj, err := sh.evalExpr(v.Value())

	if err != nil {
		return err
	}

	sh.Setvar(v.Identifier(), obj)
	return nil
}

func (sh *Shell) evalIfArguments(n *ast.IfNode) (string, string, error) {
	lvalue := n.Lvalue()
	rvalue := n.Rvalue()

	lobj, err := sh.evalExpr(lvalue)

	if err != nil {
		return "", "", err
	}

	robj, err := sh.evalExpr(rvalue)

	if err != nil {
		return "", "", err
	}

	if lobj.Type() != StringType {
		return "", "", errors.NewError("lvalue is not comparable: (%v) -> %s.", lobj, lobj.Type())
	}

	if robj.Type() != StringType {
		return "", "", errors.NewError("rvalue is not comparable")
	}

	return lobj.Str(), robj.Str(), nil
}

func (sh *Shell) executeIfEqual(n *ast.IfNode) (*Obj, error) {
	lstr, rstr, err := sh.evalIfArguments(n)

	if err != nil {
		return nil, err
	}

	if lstr == rstr {
		return sh.executeTree(n.IfTree(), false)
	} else if n.ElseTree() != nil {
		return sh.executeTree(n.ElseTree(), false)
	}

	return nil, nil
}

func (sh *Shell) executeIfNotEqual(n *ast.IfNode) (*Obj, error) {
	lstr, rstr, err := sh.evalIfArguments(n)

	if err != nil {
		return nil, err
	}

	if lstr != rstr {
		return sh.executeTree(n.IfTree(), false)
	} else if n.ElseTree() != nil {
		return sh.executeTree(n.ElseTree(), false)
	}

	return nil, nil
}

func (sh *Shell) executeFn(fn Fn, args []ast.Expr) (*Obj, error) {
	err := fn.SetArgs(args, sh)

	if err != nil {
		return nil, err
	}

	err = fn.Start()

	if err != nil {
		return nil, err
	}

	err = fn.Wait()

	if err != nil {
		return nil, err
	}

	return fn.Results(), nil
}

func (sh *Shell) executeFnInv(n *ast.FnInvNode) (*Obj, error) {
	var (
		fn Runner
		ok bool
	)

	fnName := n.Name()

	if len(fnName) > 1 && fnName[0] == '$' {
		argVar := ast.NewVarExpr(token.NewFileInfo(n.Line(), n.Column()), fnName)

		obj, err := sh.evalVariable(argVar)

		if err != nil {
			return nil, err
		}

		if obj.Type() != FnType {
			return nil, errors.NewError("Variable '%s' isnt a function.", fnName)
		}

		fn = obj.Fn()
	} else {
		fn, ok = sh.GetBuiltin(fnName)

		if !ok {
			fn, ok = sh.GetFn(fnName)

			if !ok {
				return nil, errors.NewError("no such function '%s'", fnName)
			}
		}
	}

	err := fn.SetArgs(n.Args(), sh)

	if err != nil {
		return nil, err
	}

	err = fn.Start()

	if err != nil {
		return nil, err
	}

	err = fn.Wait()

	if err != nil {
		return nil, err
	}

	return fn.Results(), nil
}

func (sh *Shell) executeInfLoop(tr *ast.Tree) error {
	var err error

	for {
		_, err = sh.executeTree(tr, true)

		runtime.Gosched()

		type interruptedError interface {
			Interrupted() bool
		}

		if errInterrupted, ok := err.(interruptedError); ok && errInterrupted.Interrupted() {
			break
		}

		sh.Lock()

		if sh.getIntr() {
			sh.setIntr(false)

			if err != nil {
				err = newErrInterrupted(err.Error())
			} else {
				err = newErrInterrupted("loop interrupted")
			}
		}

		sh.Unlock()

		if err != nil {
			break
		}
	}

	return err
}

func (sh *Shell) executeFor(n *ast.ForNode) error {
	sh.Lock()
	sh.looping = true
	sh.Unlock()

	defer func() {
		sh.Lock()
		defer sh.Unlock()

		sh.looping = false
	}()

	if n.InVar() == "" {
		return sh.executeInfLoop(n.Tree())
	}

	id := n.Identifier()
	inVar := n.InVar()

	argVar := ast.NewVarExpr(token.NewFileInfo(n.Line(), n.Column()), inVar)

	obj, err := sh.evalVariable(argVar)

	if err != nil {
		return err
	}

	if obj.Type() != ListType {
		return errors.NewError("Invalid variable type in for range: %s", obj.Type())
	}

	for _, val := range obj.List() {
		sh.Setvar(id, val)

		obj, err = sh.executeTree(n.Tree(), true)

		type interruptedError interface {
			Interrupted() bool
		}

		if errInterrupted, ok := err.(interruptedError); ok && errInterrupted.Interrupted() {
			return err
		}

		sh.Lock()

		if sh.getIntr() {
			sh.setIntr(false)
			sh.Unlock()

			if err != nil {
				return newErrInterrupted(err.Error())
			}

			return newErrInterrupted("loop interrupted")
		}

		sh.Unlock()

		if err != nil {
			return err
		}
	}

	return nil
}

func (sh *Shell) executeFnDecl(n *ast.FnDeclNode) error {
	fn, err := NewUserFn(n.Name(), sh)

	if err != nil {
		return err
	}

	fn.SetRepr(n.String())

	args := n.Args()

	for i := 0; i < len(args); i++ {
		arg := args[i]

		fn.AddArgName(arg)
	}

	fn.SetTree(n.Tree())

	fnName := n.Name()

	if fnName == "" {
		fnName = fmt.Sprintf("lambda %d", int(sh.lambdas))
		sh.lambdas++
	}

	sh.fns[fnName] = fn

	sh.Setvar(fnName, NewFnObj(fn))
	sh.logf("Function %s declared on '%s'", fnName, sh.name)

	return nil
}

func (sh *Shell) dumpVar(file io.Writer) {
	for n, v := range sh.vars {
		printVar(file, n, v)
	}
}

func (sh *Shell) dumpEnv(file io.Writer) {
	for n := range sh.env {
		printEnv(file, n)
	}
}

func (sh *Shell) dumpFns(file io.Writer) {
	for _, f := range sh.fns {
		fmt.Fprintf(file, "%s\n\n", f.String())
	}
}

func (sh *Shell) dump(out io.Writer) {
	sh.dumpVar(out)
	sh.dumpEnv(out)
	sh.dumpFns(out)
}

func (sh *Shell) executeDump(n *ast.DumpNode) error {
	var (
		err  error
		file io.Writer
		obj  *Obj
	)

	fnameArg := n.Filename()

	if fnameArg == nil {
		file = sh.stdout
		goto execDump
	}

	obj, err = sh.evalExpr(fnameArg)

	if err != nil {
		return err
	}

	if obj.Type() != StringType {
		return errors.NewError("dump does not support argument of type %s", obj.Type())
	}

	file, err = os.OpenFile(obj.Str(), os.O_CREATE|os.O_RDWR, 0644)

	if err != nil {
		return err
	}

execDump:
	sh.dump(file)

	return nil
}

func (sh *Shell) executeBindFn(n *ast.BindFnNode) error {
	if fn, ok := sh.GetFn(n.Name()); ok {
		sh.Setbindfn(n.CmdName(), fn)
	} else {
		return errors.NewError("No such function '%s'", n.Name())
	}

	return nil
}

func (sh *Shell) executeIf(n *ast.IfNode) (*Obj, error) {
	op := n.Op()

	if op == "==" {
		return sh.executeIfEqual(n)
	} else if op == "!=" {
		return sh.executeIfNotEqual(n)
	}

	return nil, fmt.Errorf("Invalid operation '%s'.", op)
}
