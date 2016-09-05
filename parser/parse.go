package parser

import (
	"fmt"
	"runtime"

	"strconv"

	"github.com/NeowayLabs/nash/ast"
	"github.com/NeowayLabs/nash/errors"
	"github.com/NeowayLabs/nash/scanner"
	"github.com/NeowayLabs/nash/token"
)

type (
	// Parser parses an nash file
	Parser struct {
		name       string // filename or name of the buffer
		content    string
		l          *scanner.Lexer
		tok        *scanner.Token // token saved for lookahead
		openblocks int

		insidePipe bool

		keywordParsers map[token.Token]parserFn
	}

	parserFn func(tok scanner.Token) (ast.Node, error)
)

// NewParser creates a new parser
func NewParser(name, content string) *Parser {
	p := &Parser{
		name:    name,
		content: content,
		l:       scanner.Lex(name, content),
	}

	p.keywordParsers = map[token.Token]parserFn{
		token.For:     p.parseFor,
		token.If:      p.parseIf,
		token.Fn:      p.parseFnDecl,
		token.Return:  p.parseReturn,
		token.Import:  p.parseImport,
		token.SetEnv:  p.parseSetenv,
		token.Rfork:   p.parseRfork,
		token.BindFn:  p.parseBindFn,
		token.Dump:    p.parseDump,
		token.Comment: p.parseComment,
		token.Illegal: p.parseError,
	}

	return p
}

// Parse starts the parsing.
func (p *Parser) Parse() (tr *ast.Tree, err error) {
	var root *ast.ListNode

	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); ok {
				panic(r)
			}

			err = r.(error)
		}
	}()

	root, err = p.parseBlock()

	if err != nil {
		return nil, err
	}

	tr = ast.NewTree(p.name)
	tr.Root = root

	return tr, nil
}

// next returns the next item from lookahead buffer if not empty or
// from the Lexer
func (p *Parser) next() scanner.Token {
	if p.tok != nil {
		t := p.tok
		p.tok = nil
		return *t
	}

	tok := <-p.l.Tokens

	if tok.Type() == token.Illegal {
		panic(errors.NewError(tok.Value()))
	}

	return tok
}

// backup puts the item into the lookahead buffer
func (p *Parser) backup(it scanner.Token) error {
	if p.tok != nil {
		return errors.NewError("only one slot for backup/lookahead")
	}

	p.tok = &it

	return nil
}

// ignores the next item
func (p *Parser) ignore() {
	if p.tok != nil {
		p.tok = nil
	} else {
		<-p.l.Tokens
	}
}

// peek gets but do not discards the next item (lookahead)
func (p *Parser) peek() scanner.Token {
	i := p.next()
	p.tok = &i
	return i
}

func (p *Parser) parseVariable() (ast.Expr, error) {
	var err error

	it := p.next()

	if it.Type() != token.Variable {
		return nil, errors.NewError("Unexpected token %v. Expected VARIABLE", it)
	}

	variable := ast.NewVarExpr(it.Pos(), it.Value())

	it = p.peek()

	if it.Type() == token.LBrack {
		p.ignore()
		it = p.next()

		if it.Type() != token.Number && it.Type() != token.Variable {
			return nil, errors.NewError("Expected number or variable in index. Found %v", it)
		}

		var index ast.Expr

		if it.Type() == token.Number {
			// only supports base10
			intval, err := strconv.Atoi(it.Value())

			if err != nil {
				return nil, err
			}

			index = ast.NewIntExpr(it.Pos(), intval)
		} else {
			p.backup(it)

			index, err = p.parseVariable()

			if err != nil {
				return nil, err
			}
		}

		it = p.next()

		if it.Type() != token.RBrack {
			return nil, errors.NewError("Unexpected token %v. Expecting ']'", it)
		}

		return ast.NewIndexExpr(variable.Position(), variable, index), nil
	}

	return variable, nil
}

func (p *Parser) parsePipe(first *ast.CommandNode) (ast.Node, error) {
	it := p.next()

	n := ast.NewPipeNode(it.Pos())

	n.AddCmd(first)

	for it = p.peek(); it.Type() == token.Ident || it.Type() == token.Arg; it = p.peek() {
		p.next()
		cmd, err := p.parseCommand(it)

		if err != nil {
			return nil, err
		}

		n.AddCmd(cmd.(*ast.CommandNode))

		if !p.insidePipe {
			break
		}
	}

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	return n, nil
}

func (p *Parser) parseCommand(it scanner.Token) (ast.Node, error) {
	n := ast.NewCommandNode(it.Pos(), it.Value())

cmdLoop:
	for {
		it = p.peek()

		switch it.Type() {
		case token.Semicolon:
			p.ignore()
			break cmdLoop // TODO: remove this label
		case token.RBrace:
			break cmdLoop
		case token.Ident, token.Arg, token.String, token.Number, token.Variable:
			arg, err := p.getArgument(true, true)

			if err != nil {
				return nil, err
			}

			n.AddArg(arg)
		case token.Plus:
			return nil, errors.NewError("%s:%d:%d: Unexpected '+'", p.name, it.Line(), it.Column())
		case token.Gt:
			p.next()
			redir, err := p.parseRedirection(it)

			if err != nil {
				return nil, err
			}

			n.AddRedirect(redir)
		case token.Pipe:
			if p.insidePipe {
				p.next()
				return n, nil
			}

			p.insidePipe = true
			return p.parsePipe(n)
		case token.EOF:
			return n, nil
		case token.Illegal:
			return nil, errors.NewError(it.Value())
		default:
			break cmdLoop
		}
	}

	if p.insidePipe {
		p.insidePipe = false
	}

	return n, nil
}

func (p *Parser) parseRedirection(it scanner.Token) (*ast.RedirectNode, error) {
	var (
		lval, rval int = ast.RedirMapNoValue, ast.RedirMapNoValue
		err        error
	)

	redir := ast.NewRedirectNode(it.Pos())

	it = p.peek()

	if !isValidArgument(it) && it.Type() != token.LBrack {
		return nil, newParserError(it, p.name, "Unexpected token: %v", it)
	}

	// [
	if it.Type() == token.LBrack {
		p.next()
		it = p.peek()

		if it.Type() != token.Number {
			return nil, newParserError(it, p.name, "Expected lefthand side of redirection map, but found '%s'",
				it.Value())
		}

		lval, err = strconv.Atoi(it.Value())

		if err != nil {
			return nil, newParserError(it, p.name, "Redirection map expects integers. Found: %s",
				it.Value())
		}

		p.next()
		it = p.peek()

		if it.Type() != token.Assign && it.Type() != token.RBrack {
			return nil, newParserError(it, p.name, "Unexpected token %v. Expecting ASSIGN or ]",
				it)
		}

		// [xxx=
		if it.Type() == token.Assign {
			p.next()
			it = p.peek()

			if it.Type() != token.Number && it.Type() != token.RBrack {
				return nil, newParserError(it, p.name, "Unexpected token %v. Expecting REDIRMAPRSIDE or ]", it)
			}

			if it.Type() == token.Number {
				rval, err = strconv.Atoi(it.Value())

				if err != nil {
					return nil, newParserError(it, p.name, "Redirection map expects integers. Found: %s", it.Value())
				}

				p.next()
				it = p.peek()
			} else {
				rval = ast.RedirMapSupress
			}
		}

		if it.Type() != token.RBrack {
			return nil, newParserError(it, p.name, "Unexpected token %v. Expecting ]", it)
		}

		// [xxx=yyy]

		redir.SetMap(lval, rval)

		p.next()
		it = p.peek()
	}

	if !isValidArgument(it) {
		if rval != ast.RedirMapNoValue || lval != ast.RedirMapNoValue {
			return redir, nil
		}

		return nil, newParserError(it, p.name, "Unexpected token %v. Expecting STRING or ARG or VARIABLE", it)
	}

	arg, err := p.getArgument(true, true)

	if err != nil {
		return nil, err
	}

	redir.SetLocation(arg)

	return redir, nil
}

func (p *Parser) parseImport(importToken scanner.Token) (ast.Node, error) {
	it := p.next()

	if it.Type() != token.Arg && it.Type() != token.String {
		return nil, newParserError(it, p.name, "Unexpected token %v. Expecting ARG or STRING", it)
	}

	var arg *ast.StringExpr

	if it.Type() == token.String {
		arg = ast.NewStringExpr(it.Pos(), it.Value(), true)
	} else if it.Type() == token.Arg {
		arg = ast.NewStringExpr(it.Pos(), it.Value(), false)
	} else {
		return nil, newParserError(it, p.name, "Parser error: Invalid token '%v' for import path", it)
	}

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	return ast.NewImportNode(importToken.Pos(), arg), nil
}

func (p *Parser) parseSetenv(it scanner.Token) (ast.Node, error) {
	pos := it.Pos()

	it = p.next()

	if it.Type() != token.Ident {
		return nil, newParserError(it, p.name, "Unexpected token %v, expected VARIABLE", it)
	}

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	return ast.NewSetenvNode(pos, it.Value()), nil
}

func (p *Parser) getArgument(allowArg, allowConcat bool) (ast.Expr, error) {
	var err error

	it := p.next()

	if !isValidArgument(it) {
		return nil, newParserError(it, p.name, "Unexpected token %v. Expected %s, %s, %s or %s",
			it, token.Ident, token.String, token.Variable, token.Arg)
	}

	firstToken := it

	var arg ast.Expr

	if firstToken.Type() == token.Variable {
		p.backup(firstToken)

		arg, err = p.parseVariable()

		if err != nil {
			return nil, err
		}
	} else if firstToken.Type() == token.String {
		arg = ast.NewStringExpr(firstToken.Pos(), firstToken.Value(), true)
	} else {
		// Arg && Ident
		arg = ast.NewStringExpr(firstToken.Pos(), firstToken.Value(), false)
	}

	it = p.peek()

	if it.Type() == token.Plus && allowConcat {
		return p.getConcatArg(arg)
	}

	if (firstToken.Type() == token.Arg || firstToken.Type() == token.Ident) && !allowArg {
		return nil, newParserError(it, p.name, "Unquoted string not allowed at pos %d (%s)", it.Pos(), it.Value())
	}

	return arg, nil
}

func (p *Parser) getConcatArg(firstArg ast.Expr) (ast.Expr, error) {
	var (
		it    scanner.Token
		parts []ast.Expr
	)

	parts = append(parts, firstArg)

hasConcat:
	it = p.peek()

	if it.Type() == token.Plus {
		p.ignore()

		arg, err := p.getArgument(true, false)

		if err != nil {
			return nil, err
		}

		parts = append(parts, arg)
		goto hasConcat
	}

	return ast.NewConcatExpr(firstArg.Position(), parts), nil
}

func (p *Parser) parseAssignment(ident scanner.Token) (ast.Node, error) {
	it := p.next()

	if it.Type() != token.Assign && it.Type() != token.AssignCmd {
		return nil, errors.NewError("Unexpected token %v, expected '=' or '<='", it)
	}

	if it.Type() == token.Assign {
		return p.parseAssignValue(ident)
	}

	return p.parseAssignCmdOut(ident)
}

func (p *Parser) parseAssignValue(name scanner.Token) (ast.Node, error) {
	var err error

	assignIdent := name

	var value ast.Expr

	it := p.peek()

	if it.Type() == token.Variable || it.Type() == token.String {
		value, err = p.getArgument(false, true)

		if err != nil {
			return nil, err
		}

	} else if it.Type() == token.LParen { // list
		lit := p.next()

		var values []ast.Expr

		it = p.peek()

		for isValidArgument(it) {
			arg, err := p.getArgument(true, true)

			if err != nil {
				return nil, err
			}

			it = p.peek()

			values = append(values, arg)
		}

		if it.Type() != token.RParen {
			return nil, errors.NewUnfinishedListError(p.name, it)
		}

		p.ignore()
		value = ast.NewListExpr(lit.Pos(), values)
	} else {
		return nil, newParserError(it, p.name, "Unexpected token %v. Expecting VARIABLE or STRING or (", it)
	}

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	return ast.NewAssignmentNode(assignIdent.Pos(), assignIdent.Value(), value), nil
}

func (p *Parser) parseAssignCmdOut(name scanner.Token) (ast.Node, error) {
	it := p.next()

	if it.Type() != token.Ident {
		return nil, errors.NewError("Invalid token %v. Expected command or function invocation", it)
	}

	nextIt := p.peek()

	if nextIt.Type() != token.LParen {
		cmd, err := p.parseCommand(it)

		if err != nil {
			return nil, err
		}

		return ast.NewExecAssignNode(name.Pos(), name.Value(), cmd)
	}

	fn, err := p.parseFnInv(it)

	if err != nil {
		return nil, err
	}

	return ast.NewExecAssignNode(name.Pos(), name.Value(), fn)
}

func (p *Parser) parseRfork(it scanner.Token) (ast.Node, error) {
	n := ast.NewRforkNode(it.Pos())

	it = p.next()

	if it.Type() != token.Ident {
		return nil, newParserError(it, p.name, "rfork requires one or more of the following flags: %s", ast.RforkFlags)
	}

	arg := ast.NewStringExpr(it.Pos(), it.Value(), false)
	n.SetFlags(arg)

	it = p.peek()

	if it.Type() == token.LBrace {
		p.ignore() // ignore lookaheaded symbol
		p.openblocks++

		tree := ast.NewTree("rfork block")
		r, err := p.parseBlock()

		if err != nil {
			return nil, err
		}

		tree.Root = r

		n.SetTree(tree)
	}

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	return n, nil
}

func (p *Parser) parseIf(it scanner.Token) (ast.Node, error) {
	n := ast.NewIfNode(it.Pos())

	it = p.peek()

	if it.Type() != token.String && it.Type() != token.Variable {
		return nil, newParserError(it, p.name, "if requires an lvalue of type string or variable. Found %v", it)
	}

	if it.Type() == token.String {
		p.next()
		arg := ast.NewStringExpr(it.Pos(), it.Value(), true)
		n.SetLvalue(arg)
	} else if it.Type() == token.Variable {
		arg, err := p.parseVariable()

		if err != nil {
			return nil, err
		}

		n.SetLvalue(arg)
	} else {
		return nil, errors.NewError("Unexpected token %v, expected %v or %v",
			it, token.String, token.Variable)
	}

	it = p.next()

	if it.Type() != token.Equal && it.Type() != token.NotEqual {
		return nil, newParserError(it, p.name, "Expected comparison, but found %v", it)
	}

	if it.Value() != "==" && it.Value() != "!=" {
		return nil, newParserError(it, p.name, "Invalid if operator '%s'. Valid comparison operators are '==' and '!='",
			it.Value())
	}

	n.SetOp(it.Value())

	it = p.next()

	if it.Type() != token.String && it.Type() != token.Variable {
		return nil, newParserError(it, p.name, "if requires an rvalue of type string or variable. Found %v", it)
	}

	if it.Type() == token.String {
		arg := ast.NewStringExpr(it.Pos(), it.Value(), true)
		n.SetRvalue(arg)
	} else {
		arg := ast.NewStringExpr(it.Pos(), it.Value(), false)
		n.SetRvalue(arg)
	}

	it = p.next()

	if it.Type() != token.LBrace {
		return nil, newParserError(it, p.name, "Expected '{' but found %v", it)
	}

	p.openblocks++

	r, err := p.parseBlock()

	if err != nil {
		return nil, err
	}

	ifTree := ast.NewTree("if block")
	ifTree.Root = r
	n.SetIfTree(ifTree)

	it = p.peek()

	if it.Type() == token.Else {
		p.next()

		elseBlock, elseIf, err := p.parseElse()

		if err != nil {
			return nil, err
		}

		elseTree := ast.NewTree("else tree")
		elseTree.Root = elseBlock

		n.SetElseif(elseIf)
		n.SetElseTree(elseTree)
	}

	return n, nil
}

func (p *Parser) parseFnArgs() ([]string, error) {
	var args []string

	if p.peek().Type() == token.RParen {
		// no argument
		p.ignore()
		return args, nil
	}

	for {
		it := p.next()

		if it.Type() == token.Ident {
			args = append(args, it.Value())
		} else {
			return nil, newParserError(it, p.name, "Unexpected token %v. Expected identifier or ')'", it)
		}

		it = p.peek()

		if it.Type() == token.Comma {
			p.ignore()

			it = p.peek()

			if it.Type() == token.RParen {
				return nil, newParserError(it, p.name, "Unexpected '%v'.", it)
			}

			continue
		}

		if it.Type() != token.RParen {
			return nil, newParserError(it, p.name, "Unexpected '%v'. Expected ')'", it)
		}

		p.ignore()

		break
	}

	return args, nil
}

func (p *Parser) parseFnDecl(it scanner.Token) (ast.Node, error) {
	n := ast.NewFnDeclNode(it.Pos(), "")

	it = p.next()

	if it.Type() == token.Ident {
		n.SetName(it.Value())

		it = p.next()
	}

	if it.Type() != token.LParen {
		return nil, errors.NewError("Unexpected token %v. Expected '('", it)
	}

	args, err := p.parseFnArgs()

	if err != nil {
		return nil, err
	}

	for _, arg := range args {
		n.AddArg(arg)
	}

	it = p.next()

	if it.Type() != token.LBrace {
		return nil, errors.NewError("Unexpected token %v. Expected '{'", it)
	}

	p.openblocks++

	tree := ast.NewTree(fmt.Sprintf("fn %s body", n.Name()))

	r, err := p.parseBlock()

	if err != nil {
		return nil, err
	}

	tree.Root = r

	n.SetTree(tree)

	return n, nil

}

func (p *Parser) parseFnInv(ident scanner.Token) (ast.Node, error) {
	n := ast.NewFnInvNode(ident.Pos(), ident.Value())

	it := p.next()

	if it.Type() != token.LParen {
		return nil, newParserError(it, p.name, "Invalid token %v. Expected '('", it)
	}

	for {
		it = p.peek()

		if it.Type() == token.String || it.Type() == token.Variable {
			arg, err := p.getArgument(false, true)

			if err != nil {
				return nil, err
			}

			n.AddArg(arg)
		} else if it.Type() == token.RParen {
			p.next()
			break
		} else {
			return nil, errors.NewError("Unexpected token %v. Expecting STRING, VARIABLE or )", it)
		}

		if p.peek().Type() == token.Comma {
			p.ignore()

			continue
		}
	}

	// semicolon is optional here
	if p.peek().Type() == token.Semicolon {
		p.next()
	}

	return n, nil
}

func (p *Parser) parseElse() (*ast.ListNode, bool, error) {
	it := p.next()

	if it.Type() == token.LBrace {
		p.openblocks++

		elseBlock, err := p.parseBlock()

		if err != nil {
			return nil, false, err
		}

		return elseBlock, false, nil
	}

	if it.Type() == token.If {
		ifNode, err := p.parseIf(it)

		if err != nil {
			return nil, false, err
		}

		block := ast.NewListNode()
		block.Push(ifNode)

		return block, true, nil
	}

	return nil, false, newParserError(it, p.name, "Unexpected token: %v", it)
}

func (p *Parser) parseBindFn(bindIt scanner.Token) (ast.Node, error) {
	nameIt := p.next()

	if nameIt.Type() != token.Ident {
		return nil, errors.NewError("Expected identifier, but found '%v'", nameIt)
	}

	cmdIt := p.next()

	if cmdIt.Type() != token.Ident {
		return nil, errors.NewError("Expected identifier, but found '%v'", cmdIt)
	}

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	n := ast.NewBindFnNode(bindIt.Pos(), nameIt.Value(), cmdIt.Value())
	return n, nil
}

func (p *Parser) parseDump(dumpIt scanner.Token) (ast.Node, error) {
	dump := ast.NewDumpNode(dumpIt.Pos())

	fnameIt := p.peek()

	var arg ast.Expr

	switch fnameIt.Type() {
	case token.Semicolon:
		p.ignore()
		return dump, nil
	case token.String:
		arg = ast.NewStringExpr(fnameIt.Pos(), fnameIt.Value(), true)
	case token.Arg:
		arg = ast.NewStringExpr(fnameIt.Pos(), fnameIt.Value(), false)
	case token.Variable:
		arg = ast.NewVarExpr(fnameIt.Pos(), fnameIt.Value())
	default:
		return dump, nil
	}

	p.ignore()

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	dump.SetFilename(arg)

	return dump, nil
}

func (p *Parser) parseReturn(retIt scanner.Token) (ast.Node, error) {
	ret := ast.NewReturnNode(retIt.Pos())

	valueIt := p.peek()

	// return;
	// return }
	// return $v
	// return "<some>"
	// return ( ... values ... )
	if valueIt.Type() != token.Semicolon &&
		valueIt.Type() != token.RBrace &&
		valueIt.Type() != token.Variable &&
		valueIt.Type() != token.String &&
		valueIt.Type() != token.LParen {
		return nil, newParserError(valueIt, p.name,
			"Expected ';', STRING, VARIABLE or LPAREN, but found %v",
			valueIt)
	}

	if valueIt.Type() == token.Semicolon {
		p.ignore()
		return ret, nil
	}

	if valueIt.Type() == token.RBrace {
		return ret, nil
	}

	if valueIt.Type() == token.LParen {
		p.ignore()

		var values []ast.Expr

		for valueIt = p.peek(); valueIt.Type() != token.RParen && valueIt.Type() != token.EOF; valueIt = p.peek() {
			arg, err := p.getArgument(true, true)

			if err != nil {
				return nil, err
			}

			values = append(values, arg)
		}

		if valueIt.Type() != token.RParen {
			return nil, errors.NewUnfinishedListError(p.name, valueIt)
		}

		p.ignore()

		if p.peek().Type() == token.Semicolon {
			p.ignore()
		}

		listArg := ast.NewListExpr(ret.Position(), values)
		ret.SetReturn(listArg)
		return ret, nil
	}

	arg, err := p.getArgument(false, true)

	if err != nil {
		return nil, err
	}

	ret.SetReturn(arg)

	if p.peek().Type() == token.Semicolon {
		p.ignore()
	}

	return ret, nil
}

func (p *Parser) parseFor(it scanner.Token) (ast.Node, error) {
	forStmt := ast.NewForNode(it.Pos())

	it = p.peek()

	if it.Type() != token.Ident {
		goto forBlockParse
	}

	p.next()

	forStmt.SetIdentifier(it.Value())

	it = p.next()

	if it.Type() != token.Ident || it.Value() != "in" {
		return nil, errors.NewError("Expected 'in' but found %q", it)
	}

	it = p.next()

	if it.Type() != token.Variable {
		return nil, errors.NewError("Expected variable but found %q", it)
	}

	forStmt.SetInVar(it.Value())
forBlockParse:
	it = p.peek()

	if it.Type() != token.LBrace {
		return nil, errors.NewError("Expected '{' but found %q", it)
	}

	p.ignore() // ignore lookaheaded symbol
	p.openblocks++

	tree := ast.NewTree("for block")

	r, err := p.parseBlock()

	if err != nil {
		return nil, err
	}

	tree.Root = r
	forStmt.SetTree(tree)

	return forStmt, nil
}

func (p *Parser) parseComment(it scanner.Token) (ast.Node, error) {
	return ast.NewCommentNode(it.Pos(), it.Value()), nil
}

func (p *Parser) parseStatement() (ast.Node, error) {
	it := p.next()
	next := p.peek()

	if fn, ok := p.keywordParsers[it.Type()]; ok {
		return fn(it)
	}

	// statement starting with ident:
	// - fn invocation
	// - variable assignment
	// - variable exec assignment
	// - Command

	if (it.Type() == token.Ident || it.Type() == token.Variable) && next.Type() == token.LParen {
		return p.parseFnInv(it)
	}

	if it.Type() == token.Ident {
		switch next.Type() {
		case token.Assign, token.AssignCmd:
			return p.parseAssignment(it)
		}

		return p.parseCommand(it)
	} else if it.Type() == token.Arg {
		return p.parseCommand(it)
	}

	return nil, newParserError(it, p.name, "Unexpected token parsing statement '%+v'", it)
}

func (p *Parser) parseError(it scanner.Token) (ast.Node, error) {
	return nil, errors.NewError(it.Value())
}

func (p *Parser) parseBlock() (*ast.ListNode, error) {
	ln := ast.NewListNode()

	for {
		it := p.peek()

		switch it.Type() {
		case token.EOF:
			goto finish
		case token.LBrace:
			p.ignore()

			return nil, errors.NewError("Parser error: Unexpected '{'")
		case token.RBrace:
			p.ignore()

			if p.openblocks <= 0 {
				return nil, errors.NewError("Parser error: No block open for close")
			}

			p.openblocks--
			return ln, nil
		default:
			n, err := p.parseStatement()

			if err != nil {
				return nil, err
			}

			ln.Push(n)
		}
	}

finish:
	if p.openblocks != 0 {
		return nil, errors.NewUnfinishedBlockError(p.name, p.peek())
	}

	return ln, nil
}

func newParserError(item scanner.Token, name, format string, args ...interface{}) error {
	if item.Type() == token.Illegal {
		// scanner error
		return errors.NewError(item.Value())
	}

	errstr := fmt.Sprintf(format, args...)

	return errors.NewError("%s:%d:%d: %s", name, item.Line(), item.Column(), errstr)
}

func isValidArgument(t scanner.Token) bool {
	if t.Type() == token.String ||
		t.Type() == token.Number ||
		t.Type() == token.Arg ||
		t.Type() == token.Ident ||
		token.IsKeyword(t.Type()) ||
		t.Type() == token.Variable {
		return true
	}

	return false
}