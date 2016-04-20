// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package sh

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
)

func Parse(r io.Reader, name string) (Prog, error) {
	p := &parser{
		r:    bufio.NewReader(r),
		name: name,
		npos: position{
			line: 1,
			col:  1,
		},
		stops: [][]Token{nil},
	}
	p.next()
	prog := p.program()
	return prog, p.err
}

type parser struct {
	r    *bufio.Reader
	name string

	err error

	spaced bool
	quote  rune

	ltok Token
	tok  Token
	lval string
	val  string

	// backup position to unread a rune
	bpos position

	lpos position
	pos  position
	npos position

	stack []interface{}
	stops [][]Token

	// to not include ')' in a literal
	quotedCmdSubst bool
}

type position struct {
	line int
	col  int
}

var reserved = map[rune]bool{
	'\n': true,
	'&':  true,
	'>':  true,
	'<':  true,
	'|':  true,
	';':  true,
	'(':  true,
	')':  true,
	'$':  true,
	'"':  true,
}

// like reserved, but these are only reserved if at the start of a word
var starters = map[rune]bool{
	'{': true,
	'}': true,
	'#': true,
}

var space = map[rune]bool{
	' ':  true,
	'\t': true,
}

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (p *parser) readRune() (rune, error) {
	r, _, err := p.r.ReadRune()
	if err != nil {
		if err == io.EOF {
			p.setEOF()
		} else {
			p.errPass(err)
		}
		return 0, err
	}
	p.moveWith(r)
	return r, nil
}

func (p *parser) moveWith(r rune) {
	p.bpos = p.npos
	if r == '\n' {
		p.npos.line++
		p.npos.col = 1
	} else {
		p.npos.col++
	}
}

func (p *parser) unreadRune() {
	if err := p.r.UnreadRune(); err != nil {
		panic(err)
	}
	p.npos = p.bpos
}

func (p *parser) readOnly(wanted rune) bool {
	// Don't use our read/unread wrappers to avoid unnecessary
	// position movement and unwanted calls to p.eof()
	r, _, err := p.r.ReadRune()
	if r == wanted {
		p.moveWith(r)
		return true
	}
	if err == nil {
		p.r.UnreadRune()
	}
	return false
}

func (p *parser) next() {
	p.lpos = p.pos
	var r rune
	p.spaced = false
	for {
		var err error
		if r, err = p.readRune(); err != nil {
			return
		}
		if p.quote != 0 || !space[r] {
			break
		}
		p.spaced = true
	}
	p.pos = p.npos
	p.pos.col--
	switch {
	case r == '\\' && p.readOnly('\n'):
		p.next()
	case reserved[r], starters[r]:
		// Between double quotes, only under certain
		// circumstnaces do we tokenize
		if p.quote == '"' {
			switch {
			case r == '"', r == '$':
			case p.tok == EXP:
			case r == ')' && p.quotedCmdSubst:
			default:
				p.advance(LIT, p.readLit(r))
				return
			}
		}
		switch r {
		case '#':
			p.advance(COMMENT, p.readLine())
		case '\n':
			p.advance('\n', "")
		default:
			p.advance(doToken(r, p.readOnly), "")
		}
	default:
		p.advance(LIT, p.readLit(r))
	}
}

func (p *parser) readLit(r rune) string {
	return string(p.readLitRunes(r))
}

func (p *parser) readLitRunes(r rune) (rs []rune) {
	for {
		appendRune := true
		switch {
		case p.quote != '\'' && r == '\\': // escaped rune
			r, _ = p.readRune()
			if r != '\n' {
				rs = append(rs, '\\', r)
			}
			appendRune = false
		case p.quote != '\'' && r == '$': // end of lit
			p.unreadRune()
			return
		case p.quote == '"':
			if r == p.quote || (p.quotedCmdSubst && r == ')') {
				p.unreadRune()
				return
			}
		case p.quote == '\'':
			if r == p.quote {
				p.quote = 0
			}
		case r == '\'':
			p.quote = '\''
		case reserved[r], space[r]: // end of lit
			p.unreadRune()
			return
		}
		if appendRune {
			rs = append(rs, r)
		}
		var err error
		if r, err = p.readRune(); err != nil {
			if p.quote != 0 {
				p.errWanted(Token(p.quote))
			}
			break
		}
	}
	return
}

func (p *parser) advance(tok Token, val string) {
	if p.tok != EOF {
		p.ltok = p.tok
		p.lval = p.val
	}
	p.tok = tok
	p.val = val
}

func (p *parser) setEOF() {
	p.advance(EOF, "EOF")
}

func (p *parser) readUntil(tok Token) (string, bool) {
	var rs []rune
	for {
		r, err := p.readRune()
		if err != nil {
			return string(rs), false
		}
		if tok == doToken(r, p.readOnly) {
			return string(rs), true
		}
		rs = append(rs, r)
	}
}

func (p *parser) readUntilWant(tok Token) string {
	s, found := p.readUntil(tok)
	if !found {
		p.errWanted(tok)
	}
	return s
}

func (p *parser) readLine() string {
	s, _ := p.readUntil('\n')
	return s
}

// We can't simply have these as tokens as they can sometimes be valid
// words, e.g. `echo if`.
var reservedLits = map[Token]string{
	IF:    "if",
	THEN:  "then",
	ELIF:  "elif",
	ELSE:  "else",
	FI:    "fi",
	WHILE: "while",
	FOR:   "for",
	IN:    "in",
	DO:    "do",
	DONE:  "done",
	CASE:  "case",
	ESAC:  "esac",
}

func (p *parser) peek(tok Token) bool {
	return p.tok == tok || (p.tok == LIT && p.val == reservedLits[tok])
}

func (p *parser) got(tok Token) bool {
	if p.peek(tok) {
		p.next()
		return true
	}
	return false
}

func (p *parser) want(tok Token) {
	if !p.peek(tok) {
		p.errWanted(tok)
		return
	}
	p.next()
}

func (p *parser) errPass(err error) {
	if p.err == nil {
		p.err = err
	}
	p.setEOF()
}

type lineErr struct {
	fname string
	pos   position
	text  string
}

func (e lineErr) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.fname, e.pos.line, e.pos.col, e.text)
}

func (p *parser) posErr(pos position, format string, v ...interface{}) {
	p.errPass(lineErr{
		fname: p.name,
		pos:   pos,
		text:  fmt.Sprintf(format, v...),
	})
}

func (p *parser) curErr(format string, v ...interface{}) {
	if p.tok == EOF {
		p.pos = p.npos
	}
	p.posErr(p.pos, format, v...)
}

func (p *parser) errWantedStr(s string) {
	p.curErr("unexpected token %s - wanted %s", p.tok, s)
}

func (p *parser) errWanted(tok Token) {
	p.errWantedStr(tok.String())
}

func (p *parser) errAfterStr(s string) {
	p.curErr("unexpected token %s after %s", p.tok, s)
}

func (p *parser) add(n Node) {
	cur := p.stack[len(p.stack)-1]
	switch x := cur.(type) {
	case *[]Node:
		*x = append(*x, n)
	case *Node:
		if *x != nil {
			panic("single node set twice")
		}
		*x = n
	default:
		panic("unknown type in the stack")
	}
}

func (p *parser) pop() {
	p.stack = p.stack[:len(p.stack)-1]
}

func (p *parser) push(v interface{}) {
	p.stack = append(p.stack, v)
}

func (p *parser) popAdd(n Node) {
	p.pop()
	p.add(n)
}

func (p *parser) program() (pr Prog) {
	p.commands(&pr.Stmts)
	return
}

func (p *parser) commands(stmts *[]Node, stop ...Token) int {
	return p.commandsPropagating(false, stmts, stop...)
}

func (p *parser) commandsLimited(stmts *[]Node, stop ...Token) int {
	return p.commandsPropagating(true, stmts, stop...)
}

func (p *parser) commandsPropagating(propagate bool, stmts *[]Node, stop ...Token) (count int) {
	if propagate {
		p.stops = append(p.stops, stop)
		defer func() {
			p.stops = p.stops[:len(p.stops)-1]
		}()
	}
	var n Node
	for p.tok != EOF {
		for _, tok := range stop {
			if p.peek(tok) {
				return
			}
		}
		if !p.gotCommand(&n) && p.tok != EOF {
			p.errWantedStr("command")
			break
		}
		if n != nil {
			*stmts = append(*stmts, n)
		}
		count++
	}
	return
}

func (p *parser) getWord() (w Word) {
	p.push(&w.Parts)
	if p.readParts() == 0 {
		p.errWantedStr("word")
	}
	p.pop()
	return
}

func (p *parser) word() {
	p.add(p.getWord())
}

func (p *parser) getLit() Lit {
	p.want(LIT)
	return Lit{Val: p.lval}
}

func (p *parser) lit() {
	p.add(p.getLit())
}

func (p *parser) readParts() (count int) {
	for p.tok != EOF {
		switch {
		case p.quote == 0 && count > 0 && p.spaced:
			return
		case p.peek(LIT):
			p.lit()
		case p.quote == 0 && p.peek('"'):
			var dq DblQuoted
			p.quote = '"'
			p.next()
			p.push(&dq.Parts)
			p.readParts()
			p.pop()
			p.quote = 0
			p.want('"')
			p.add(dq)
		case p.got(EXP):
			switch {
			case p.peek(LBRACE):
				p.add(ParamExp{Text: p.readUntilWant(RBRACE)})
				p.next()
			case p.got(LIT):
				p.add(ParamExp{Short: true, Text: p.lval})
			case p.peek(DLPAREN):
				p.add(ArithmExp{Text: p.readUntilWant(DRPAREN)})
				p.next()
			case p.peek(LPAREN):
				var cs CmdSubst
				p.quotedCmdSubst = p.quote == '"'
				p.next()
				p.commandsLimited(&cs.Stmts, RPAREN)
				p.quotedCmdSubst = false
				p.add(cs)
				p.want(RPAREN)
			}
		default:
			return
		}
		count++
	}
	return
}

func (p *parser) wordList() (count int) {
	var stop = [...]Token{SEMICOLON, '\n'}
	for p.tok != EOF {
		for _, tok := range stop {
			if p.got(tok) {
				return
			}
		}
		p.word()
		count++
	}
	return
}

func (p *parser) gotEnd() bool {
	if p.tok == EOF || p.got(SEMICOLON) || p.got('\n') || p.got(COMMENT) {
		return true
	}
	stop := p.stops[len(p.stops)-1]
	for _, tok := range stop {
		if p.peek(tok) {
			return true
		}
	}
	return false
}

func (p *parser) gotCommand(n *Node) bool {
	for p.got(COMMENT) || p.got('\n') {
	}
	switch {
	case p.peek(LPAREN):
		*n = p.subshell()
	case p.peek(LBRACE):
		*n = p.block()
	case p.peek(IF):
		*n = p.ifStmt()
	case p.peek(WHILE):
		*n = p.whileStmt()
	case p.peek(FOR):
		*n = p.forStmt()
	case p.peek(CASE):
		*n = p.caseStmt()
	case p.peek(LIT), p.peek(EXP), p.peek('\''), p.peek('"'):
		*n = p.baseCmd()
		return true
	default:
		return false
	}
	if !p.gotEnd() {
		p.errAfterStr("statement")
	}
	return true
}

func (p *parser) binaryExpr(op Token, left Node) (b BinaryExpr) {
	b.Op = op
	if !p.gotCommand(&b.Y) {
		p.curErr("%s must be followed by a command", op)
	}
	b.X = left
	p.pop()
	return
}

func (p *parser) gotRedirect() bool {
	var r Redirect
	switch {
	case p.got(RDROUT), p.got(APPEND), p.got(RDRIN):
		r.Op = p.ltok
		r.Obj = p.getWord()
	default:
		return false
	}
	p.add(r)
	return true
}

func (p *parser) subshell() (s Subshell) {
	p.want(LPAREN)
	if p.commandsLimited(&s.Stmts, RPAREN) == 0 {
		p.errWantedStr("command")
	}
	p.want(RPAREN)
	return
}

func (p *parser) block() (b Block) {
	p.want(LBRACE)
	if p.commands(&b.Stmts, RBRACE) == 0 {
		p.errWantedStr("command")
	}
	p.want(RBRACE)
	return
}

func (p *parser) ifStmt() (ifs IfStmt) {
	p.want(IF)
	if !p.gotCommand(&ifs.Cond) {
		p.curErr(`"if" must be followed by a command`)
	}
	if !p.got(THEN) {
		p.curErr(`"if x" must be followed by "then"`)
	}
	p.commands(&ifs.ThenStmts, FI, ELIF, ELSE)
	p.push(&ifs.Elifs)
	for p.got(ELIF) {
		var elf Elif
		if !p.gotCommand(&elf.Cond) {
			p.curErr(`"elif" must be followed by a command`)
		}
		if !p.got(THEN) {
			p.curErr(`"elif x" must be followed by "then"`)
		}
		p.commands(&elf.ThenStmts, FI, ELIF, ELSE)
		p.add(elf)
	}
	p.pop()
	if p.got(ELSE) {
		p.commands(&ifs.ElseStmts, FI)
	}
	if !p.got(FI) {
		p.curErr(`if statement must end with "fi"`)
	}
	return
}

func (p *parser) whileStmt() (ws WhileStmt) {
	p.want(WHILE)
	if !p.gotCommand(&ws.Cond) {
		p.curErr(`"while" must be followed by a command`)
	}
	if !p.got(DO) {
		p.curErr(`"while x" must be followed by "do"`)
	}
	p.commands(&ws.DoStmts, DONE)
	if !p.got(DONE) {
		p.curErr(`while statement must end with "done"`)
	}
	return
}

func (p *parser) forStmt() (fs ForStmt) {
	p.want(FOR)
	fs.Name = p.getLit()
	p.want(IN)
	p.push(&fs.WordList)
	p.wordList()
	p.pop()
	p.want(DO)
	p.commands(&fs.DoStmts, DONE)
	p.want(DONE)
	return
}

func (p *parser) caseStmt() (cs CaseStmt) {
	p.want(CASE)
	cs.Word = p.getWord()
	p.want(IN)
	p.push(&cs.Patterns)
	p.patterns()
	p.pop()
	p.want(ESAC)
	return
}

func (p *parser) patterns() {
	count := 0
	for p.tok != EOF && !p.peek(ESAC) {
		for p.got('\n') {
		}
		var cp CasePattern
		p.push(&cp.Parts)
		for p.tok != EOF {
			p.word()
			if p.got(RPAREN) {
				break
			}
			p.want(OR)
		}
		p.pop()
		p.commandsLimited(&cp.Stmts, DSEMICOLON, ESAC)
		p.add(cp)
		count++
		if !p.got(DSEMICOLON) {
			break
		}
		for p.got('\n') {
		}
	}
	if count == 0 {
		p.errWantedStr("pattern")
	}
}

func (p *parser) baseCmd() Node {
	fpos := p.pos
	w := p.getWord()
	if p.peek(LPAREN) {
		return p.funcDecl(w.String(), fpos)
	}
	cmd := Command{Args: []Node{w}}
	p.push(&cmd.Args)
args:
	for !p.gotEnd() {
		switch {
		case p.peek(LIT), p.peek(EXP), p.peek('\''), p.peek('"'):
			p.word()
		case p.got(LAND), p.got(OR), p.got(LOR):
			return p.binaryExpr(p.ltok, cmd)
		case p.gotRedirect():
		case p.got(AND):
			cmd.Background = true
			break args
		default:
			p.errAfterStr("command")
		}
	}
	p.pop()
	return cmd
}

func (p *parser) funcDecl(name string, pos position) (fd FuncDecl) {
	p.want(LPAREN)
	p.want(RPAREN)
	if !identRe.MatchString(name) {
		p.posErr(pos, "invalid func name: %s", name)
	}
	fd.Name.Val = name
	if !p.gotCommand(&fd.Body) {
		p.curErr(`"foo()" must be followed by a statement`)
	}
	return
}
