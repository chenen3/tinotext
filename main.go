package main

import (
	"bufio"
	"bytes"
	"container/list"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

const (
	focusEditor = iota
	focusConsole
)

type App struct {
	s       *State
	tab     View
	editor  []*View // viewport
	status  View
	console View
	cmdCh   chan string
	done    chan struct{}
}

type State struct {
	tabs          []string
	tabIdx        int
	lines         *list.List
	row           int    // Current row position (starts from 0)
	col           int    // Current column position (starts from 0)
	scroll        int    // Scroll position for the editor (starts from 0)
	upDownCol     int    // Column to maintain while navigating up/down
	status        string // Status message displayed in the status bar
	console       string // Console input text
	consoleCursor int    // Cursor position in the console
	focus         int    // Current focus (editor or console)

	symbols      map[string][]Symbol // symbol name to list of symbols
	matchSymbols []Symbol
	matchIdx     int
	completion   string

	selecting bool
	selection *selection

	undoStack []Edit
	redoStack []Edit
	lastEdit  *Edit
}

type selection struct {
	startRow int
	startCol int
	endRow   int
	endCol   int
}

type Edit struct {
	row     int
	col     int
	oldText string
	newText string
	kind    int
	time    time.Time
}

const (
	editInsert = iota
	editDelete
)

// line returns the list element at the specified line index, or nil if out of bounds.
func (st *State) line(i int) *list.Element {
	if st.lines.Len() == 0 || i > st.lines.Len()-1 {
		return nil
	}

	e := st.lines.Front()
	for range i {
		e = e.Next()
	}
	return e
}

func (st *State) switchTab(i int) {
	if i < 0 || i > len(st.tabs)-1 {
		return
	}

	st.tabIdx = i
	st.lines.Init()
	st.row = 0
	st.col = 0
	st.scroll = 0

	file := st.tabs[st.tabIdx]
	if file == "" {
		return
	}

	f, err := os.Open(file)
	if err != nil {
		log.Print(err)
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		st.lines.PushBack(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Print(err)
		return
	}
}

type View struct {
	x, y, w, h int
	style      tcell.Style
}

// draw draws a line and clears the remaining space
func (v *View) draw(line string) {
	for row := range v.h {
		for col := range v.w {
			if col < len(line) {
				screen.SetContent(v.x+col, v.y+row, rune(line[col]), nil, v.style)
			} else {
				screen.SetContent(v.x+col, v.y+row, ' ', nil, v.style)
			}
		}
	}
}

// drawEditorLine draws a line with automatic tab expansion for editor content
func (v *View) drawEditorLine(line string) {
	v.draw(expandTabs(line))
}

type textStyle struct {
	text  string
	style tcell.Style
}

// drawText draw inline texts with multiple styles.
// Note that it does not handle tab expansion.
func (v *View) drawText(texts ...textStyle) {
	var col int
	for _, ts := range texts {
		style := ts.style
		if style == tcell.StyleDefault {
			style = v.style
		}
		for _, c := range ts.text {
			screen.SetContent(v.x+col, v.y, c, nil, style)
			col++
		}
	}
	// clear remaining space
	for i := col; i < v.w; i++ {
		screen.SetContent(v.x+i, v.y, ' ', nil, v.style)
	}
}

func (v *View) contains(x, y int) bool {
	return x >= v.x && x < v.x+v.w && y >= v.y && y < v.y+v.h
}

func (a *App) resize() {
	w, h := screen.Size()
	a.tab = View{0, 0, w, 1, tcell.StyleDefault.Reverse(true)}
	a.editor = make([]*View, h-3)
	for i := range a.editor {
		a.editor[i] = &View{0, i + a.tab.h, w, 1, tcell.StyleDefault}
	}
	a.status = View{0, h - 2, w, 1, tcell.StyleDefault.Reverse(true)}
	a.console = View{0, h - 1, w, 1, tcell.StyleDefault}
}

const tabSize = 4

// indentation returns the leading whitespace of a line
func indentation(line string) string {
	var indent strings.Builder
	for _, char := range line {
		if char == ' ' || char == '\t' {
			indent.WriteRune(char)
		} else {
			break
		}
	}
	return indent.String()
}

// expandTabs converts all tabs in a line to spaces for display
func expandTabs(line string) string {
	var result strings.Builder
	for _, char := range line {
		if char == '\t' {
			// Add spaces to reach the next tab stop
			spaces := tabSize - (result.Len() % tabSize)
			result.WriteString(strings.Repeat(" ", spaces))
		} else {
			result.WriteRune(char)
		}
	}
	return result.String()
}

// columnToScreen converts a column position in the original line to the screen line
func columnToScreen(line string, col int) int {
	if col > len(line) {
		col = len(line)
	}

	screenCol := 0
	for i, char := range line {
		if i >= col {
			break
		}
		if char == '\t' {
			spaces := tabSize - (screenCol % tabSize)
			screenCol += spaces
		} else {
			screenCol++
		}
	}
	return screenCol
}

// columnFromScreen converts a column position in the screen line back to the original line
func columnFromScreen(line string, screenCol int) int {
	if screenCol <= 0 {
		return 0
	}

	originalCol := 0
	currentScreenCol := 0

	for i, char := range line {
		if currentScreenCol >= screenCol {
			return originalCol
		}

		if char == '\t' {
			spaces := tabSize - (currentScreenCol % tabSize)
			currentScreenCol += spaces
		} else {
			currentScreenCol++
		}
		originalCol = i + 1
	}

	return originalCol
}

// draw the whole layout and cursor
func (a *App) draw() {
	a.drawTabs()
	a.drawEditor()
	a.status.draw(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	a.console.draw(a.s.console)
	a.syncCursor()
}

const (
	tabCloser = " x|"
	labelOpen = " Open "
	labelSave = " Save "
	labelQuit = " Quit "
)

func (a *App) drawTabs() {
	var ts []textStyle
	var totalTabWidth int
	for i, tab := range a.s.tabs {
		if tab == "" {
			tab = "untitled"
		}
		if i == a.s.tabIdx {
			ts = append(ts, textStyle{text: tab, style: a.tab.style.Bold(true).Underline(true).Italic(true)})
			ts = append(ts, textStyle{text: tabCloser, style: a.tab.style.Bold(true)})
		} else {
			ts = append(ts, textStyle{text: tab})
			ts = append(ts, textStyle{text: tabCloser})
		}
		totalTabWidth += len(tab) + len(tabCloser)
	}
	padding := max(0, a.tab.w-totalTabWidth-len(labelOpen)-len(labelSave)-len(labelQuit))
	ts = append(ts, textStyle{text: strings.Repeat(" ", padding)})
	ts = append(ts, textStyle{text: labelOpen})
	ts = append(ts, textStyle{text: labelSave})
	ts = append(ts, textStyle{text: labelQuit})
	a.tab.drawText(ts...)
}

func (a *App) drawEditor() {
	e := a.s.lines.Front()
	for range a.s.scroll {
		e = e.Next()
	}
	remainLines := a.s.lines.Len() - a.s.scroll

	var startRow, startCol, endRow, endCol int
	if a.s.selecting {
		startRow, startCol = a.s.selection.startRow, a.s.selection.startCol
		endRow, endCol = a.s.selection.endRow, a.s.selection.endCol
		if startRow > endRow {
			startRow, endRow = endRow, startRow
			startCol, endCol = endCol, startCol
		}
	}

	for i, lineView := range a.editor {
		if i >= remainLines {
			lineView.draw("")
			continue
		}

		line := e.Value.(string)
		e = e.Next()
		if !a.s.selecting || endRow < a.s.scroll+i || a.s.scroll+i < startRow {
			lineView.drawEditorLine(line)
			continue
		}

		screenLine := expandTabs(line)

		// For selections, convert positions from original line to screen line
		start, end := 0, len(screenLine)
		if startRow == a.s.scroll+i {
			start = columnToScreen(line, startCol)
		}
		if endRow == a.s.scroll+i {
			end = columnToScreen(line, endCol)
		}
		if start > end {
			start, end = end, start
		}
		lineView.drawText(
			textStyle{text: screenLine[:start]},
			textStyle{text: screenLine[start:end], style: highlightStyle},
			textStyle{text: screenLine[end:]},
		)
	}
}

var screen tcell.Screen

func main() {
	output := os.Getenv("SEYI_LOG_FILE")
	if output == "" {
		log.SetOutput(io.Discard)
	} else {
		f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file %s: %v", output, err)
		}
		defer f.Close()
		log.SetOutput(f)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	// Initialize screen
	s, err := tcell.NewScreen()
	if err != nil {
		log.Fatalf("%+v", err)
	}
	if err := s.Init(); err != nil {
		log.Fatalf("%+v", err)
	}
	defStyle := tcell.StyleDefault.Background(tcell.ColorReset).Foreground(tcell.ColorReset)
	s.SetStyle(defStyle)
	s.SetCursorStyle(tcell.CursorStyleBlinkingBlock)
	s.EnableMouse()
	s.EnablePaste()
	s.Clear()
	screen = s

	quit := func() {
		// You have to catch panics in a defer, clean up, and
		// re-raise them - otherwise your application can
		// die without leaving any diagnostic trace.
		maybePanic := recover()
		s.Fini()
		if maybePanic != nil {
			panic(maybePanic)
		}
	}
	defer quit()

	app := &App{
		s:     &State{lines: list.New(), tabs: []string{""}},
		cmdCh: make(chan string, 1),
		done:  make(chan struct{}),
	}
	go app.commandLoop()

	if len(os.Args) >= 2 {
		bs, err := os.ReadFile(os.Args[1])
		if err != nil {
			log.Print(err)
		} else {
			app.s.tabs = []string{os.Args[1]}
			for _, line := range bytes.Split(bs, []byte{'\n'}) {
				app.s.lines.PushBack(string(line))
			}
		}
	}

	eventCh := make(chan tcell.Event, 10)
	go s.ChannelEvents(eventCh, app.done)

	for {
		// Update screen
		s.Show()
		select {
		case <-app.done:
			return
		case ev := <-eventCh:
			switch ev := ev.(type) {
			case *tcell.EventResize: // arrive when the app start
				app.resize()
				app.draw()
				s.Sync()
			case *tcell.EventKey:
				log.Printf("Key pressed: %s %c", tcell.KeyNames[ev.Key()], ev.Rune())
				if ev.Key() == tcell.KeyCtrlQ {
					close(app.done)
					return
				}
				// redraw the screen, sometimes iTerm2 resize but doesn't trigger a resize event
				if ev.Key() == tcell.KeyCtrlL {
					s.Sync()
					continue
				}
				if ev.Key() == tcell.KeyCtrlW {
					app.s.closeTab(app.s.tabIdx)
					if len(app.s.tabs) == 0 {
						close(app.done)
						return
					}
					app.draw()
					continue
				}
				if ev.Key() == tcell.KeyCtrlO {
					app.s.focus = focusConsole
					app.setStatus("open file (:<line> or @symbol)")
					app.setConsole("")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlG {
					app.s.focus = focusConsole
					app.setStatus("go to line number")
					app.setConsole(":")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlR {
					app.s.focus = focusConsole
					app.setStatus("go to symbol")
					app.setConsole("@")
					app.syncCursor()

					if len(app.s.tabs) == 0 {
						app.setStatus("Not a Go file, cannot parse symbols")
						continue
					}
					if !strings.HasSuffix(app.s.tabs[app.s.tabIdx], ".go") {
						app.setStatus("Not a Go file, cannot parse symbols")
						continue
					}
					symbols, err := ParseSymbol(app.s.tabs[app.s.tabIdx])
					if err != nil {
						app.setStatus("Failed to parse symbols: " + err.Error())
						continue
					}
					app.s.symbols = symbols
					app.s.matchSymbols = nil
					app.s.matchIdx = 0
					continue
				}
				if ev.Key() == tcell.KeyCtrlF {
					app.s.focus = focusConsole
					app.setStatus("search text")
					app.setConsole("#")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlP {
					app.s.focus = focusConsole
					app.setStatus("command:")
					app.setConsole(">")
					app.syncCursor()
					continue
				}
				if ev.Key() == tcell.KeyCtrlS {
					app.cmdCh <- ">save " + app.s.tabs[app.s.tabIdx]
					continue
				}

				if app.s.focus == focusConsole {
					app.consoleEvent(ev)
					app.syncCursor()
					continue
				}
				app.editorEvent(ev)
			case *tcell.EventMouse:
				x, y := ev.Position()
				switch ev.Buttons() {
				case tcell.Button1:
					// will receive this event many times
					// when pressing left button and moving the mouse
					app.handleClick(x, y)
				case tcell.ButtonNone:
					// will receive this event when mouse is released
					// or when mouse is moved without pressing any button
					if !app.s.selecting {
						continue
					}
					app.s.selecting = false
				case tcell.WheelUp:
					app.s.scroll -= int(float32(y) * scrollFactor)
					if app.s.scroll < 0 {
						app.s.scroll = 0
					}
					app.drawEditor()
					app.syncCursor()
				case tcell.WheelDown:
					// keep in viewport
					if app.s.lines.Len() < len(app.editor) {
						app.s.scroll = 0
						continue
					}
					app.s.scroll += int(float32(y) * scrollFactor)
					if app.s.scroll > app.s.lines.Len()-len(app.editor) {
						app.s.scroll = app.s.lines.Len() - len(app.editor)
						continue
					}
					app.drawEditor()
					app.syncCursor()
				}
			}
		}
	}
}

// A multiplier to be used on scrolling
const scrollFactor = 0.1

func (a *App) handleClick(x, y int) {
	if a.tab.contains(x, y) {
		var totalTabWidth int
		for _, tab := range a.s.tabs {
			if tab == "" {
				tab = "untitled"
			}
			totalTabWidth += len(tab) + len(tabCloser)
		}

		// special labels area
		specialLabelsStart := a.tab.x + a.tab.w - len(labelOpen) - len(labelSave) - len(labelQuit)

		// Check if click is in special labels area
		if x >= specialLabelsStart {
			// Open label
			if x < specialLabelsStart+len(labelOpen) {
				a.s.focus = focusConsole
				a.setStatus("open file")
				a.setConsole("")
				a.syncCursor()
				return
			}
			// Save label
			if x < specialLabelsStart+len(labelOpen)+len(labelSave) {
				if len(a.s.tabs) > 0 && a.s.tabIdx < len(a.s.tabs) {
					a.cmdCh <- ">save " + a.s.tabs[a.s.tabIdx]
				}
				return
			}
			// Quit label
			if x < specialLabelsStart+len(labelOpen)+len(labelSave)+len(labelQuit) {
				close(a.done)
				return
			}
			return
		}

		// Check tabs - only if click is in tabs area (not in special labels)
		if x < a.tab.x+totalTabWidth {
			var currentWidth int
			for i, tab := range a.s.tabs {
				if tab == "" {
					tab = "untitled"
				}

				tabStart := a.tab.x + currentWidth
				tabEnd := tabStart + len(tab)
				closerEnd := tabEnd + len(tabCloser)

				// Check if click is within this tab's area
				if x >= tabStart && x < closerEnd {
					if x < tabEnd {
						// Clicked on tab name - switch tab
						if i != a.s.tabIdx {
							a.s.switchTab(i)
							a.s.focus = focusEditor
							a.draw()
						}
					} else {
						// Clicked on tab closer - close tab
						a.s.closeTab(i)
						if len(a.s.tabs) == 0 {
							close(a.done)
							return
						}
						a.s.focus = focusEditor
						a.draw()
					}
					return
				}

				currentWidth += len(tab) + len(tabCloser)
			}
		}
		return
	}

	if a.console.contains(x, y) {
		a.s.focus = focusConsole
		a.setConsole("")
		a.syncCursor()
		return
	}

	a.s.focus = focusEditor
	if a.s.lines.Len() == 0 {
		a.s.row = 0
		a.s.col = 0
	} else {
		a.s.row = min(y-a.editor[0].y+a.s.scroll, a.s.lines.Len()-1)
		line := a.s.line(a.s.row).Value.(string)
		screenCol := x - a.editor[0].x
		// Convert screen position back to original line position
		a.s.col = min(len(line), columnFromScreen(line, screenCol))
	}

	if !a.s.selecting {
		a.s.selection = &selection{
			startRow: a.s.row,
			startCol: a.s.col,
			endRow:   a.s.row,
			endCol:   a.s.col,
		}
		a.s.selecting = true
	} else {
		// draging the selection
		a.s.selection.endRow = a.s.row
		a.s.selection.endCol = a.s.col
	}

	a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
	a.syncCursor()
	a.drawEditor()
	a.s.upDownCol = -1 // reset up/down column tracking
	// debug
	// if line := a.s.line(a.s.row); line != nil {
	// 	log.Printf("Clicked line %d, column %d, text: %q", a.s.row+1, a.s.col+1,
	// 		line.Value.(string))
	// }
}

// setStatus updates the status view with the given string.
func (a *App) setStatus(s string) {
	a.s.status = s
	a.status.draw(s)
}

// setConsole updates the console view with the given string.
func (a *App) setConsole(s string) {
	a.s.console = s
	a.s.consoleCursor = len(s)
	a.console.draw(s)
}

// jump moves the cursor to the specified line and column,
// adjusting the scroll position to keep the line in view.
// Note: it does not render the editor
func (a *App) jump(row, col int) {
	a.s.row = row
	a.s.col = col
	a.s.scroll = max(0, a.s.row-(len(a.editor)/2)) // viewport center
}

func (a *App) consoleEvent(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape:
		a.s.console = ""
		a.s.consoleCursor = 0
		a.s.focus = focusEditor
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
		// reset matched text
		if line := a.s.line(a.s.row); line != nil {
			a.editor[a.s.row-a.s.scroll].drawEditorLine(line.Value.(string))
		}
	case tcell.KeyEnter:
		if a.s.console == "" {
			return
		}
		// go to symbol
		if a.s.console[0] == '@' && len(a.s.matchSymbols) > 0 {
			matched := a.s.matchSymbols[a.s.matchIdx]
			a.jump(matched.Line-1, matched.Column-1)
			a.s.focus = focusEditor
			a.s.console = ""
			a.draw()
			return
		}
		a.cmdCh <- strings.TrimSpace(a.s.console)
		a.s.console = ""
		a.s.consoleCursor = 0
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if a.s.console == "" {
			return
		}
		a.s.console = a.s.console[:a.s.consoleCursor-1] + a.s.console[a.s.consoleCursor:]
		a.s.consoleCursor--
		if a.s.console != "" && a.s.console[0] == '@' {
			a.cmdCh <- a.s.console
		}
	case tcell.KeyLeft:
		if a.s.console == "" {
			return
		}
		a.s.consoleCursor--
	case tcell.KeyRight:
		if a.s.console == "" || a.s.consoleCursor >= len(a.s.console) {
			return
		}
		a.s.consoleCursor++
	case tcell.KeyRune:
		if a.s.consoleCursor >= len(a.s.console) {
			a.s.console += string(ev.Rune())
		} else {
			a.s.console = a.s.console[:a.s.consoleCursor] + string(ev.Rune()) + a.s.console[a.s.consoleCursor:]
		}
		a.s.consoleCursor++
		// incremental command handling
		if len(a.s.console) >= 2 && a.s.console[0] == '@' {
			a.cmdCh <- a.s.console
		}
	case tcell.KeyTAB, tcell.KeyBacktab:
		if len(a.s.matchSymbols) == 0 {
			return
		}
		// select symbol in the match list
		if ev.Key() == tcell.KeyTAB {
			a.s.matchIdx = (a.s.matchIdx + 1) % len(a.s.matchSymbols)
		} else {
			a.s.matchIdx = (a.s.matchIdx - 1 + len(a.s.matchSymbols)) % len(a.s.matchSymbols)
		}
		ts := make([]textStyle, len(a.s.matchSymbols))
		for i, sym := range a.s.matchSymbols {
			text := sym.Name + " "
			if sym.Receiver != "" {
				text = sym.Receiver + "." + sym.Name + " "
			}
			if i == a.s.matchIdx {
				ts[i] = textStyle{text: text, style: highlightStyle}
			} else {
				ts[i] = textStyle{text: text}
			}
		}
		a.status.drawText(ts...)
	default:
		return
	}
	a.console.draw(a.s.console)
}

// closeTab closes the tab at the specified index and adjusts the current tab selection.
// It handles edge cases for tab index management and ensures a valid tab remains active.
func (st *State) closeTab(index int) {
	if index < 0 || index >= len(st.tabs) {
		return
	}

	st.tabs = slices.Delete(st.tabs, index, index+1)
	if len(st.tabs) == 0 {
		st.tabIdx = 0
		return
	}

	switch {
	case index < st.tabIdx:
		// Closed tab was before current tab, shift index left
		st.tabIdx--
	case index == st.tabIdx:
		// Closed the current tab, need to select a new one
		if st.tabIdx >= len(st.tabs) {
			st.tabIdx = len(st.tabs) - 1
		}
		// Switch to the tab now at the current index (could be same position, new tab)
		st.switchTab(st.tabIdx)
	case index > st.tabIdx:
		// Closed tab was after current tab, no index adjustment needed
	}
}

// handleCommand processes a command string and performs actions based on its prefix.
// Commands:
// - <filename> open file
// - :<line_number> go to line
// - @<symbol> go to symbol
// - #<text> search text
func (a *App) handleCommand(cmd string) {
	// this function is called outside the main goroutine,
	// so ensure to call screen.Show() after making changes to reflect updates.
	defer screen.Show()
	cmd = strings.TrimSpace(cmd)
	switch cmd[0] {
	case '>':
		c := strings.Split(cmd[1:], " ")
		switch c[0] {
		case "save":
			if len(c) == 1 || len(c[1]) == 0 {
				a.setStatus("Usage: >save <filename>")
				a.setConsole(">save ")
				a.s.focus = focusConsole
				a.syncCursor()
				return
			}
			filename := c[1]
			var content []string
			for e := a.s.lines.Front(); e != nil; e = e.Next() {
				content = append(content, e.Value.(string))
			}
			err := os.WriteFile(filename, []byte(strings.Join(content, "\n")), 0644)
			if err != nil {
				log.Printf("Failed to save file %s: %v", filename, err)
				a.setStatus("Failed to save file: " + err.Error())
			} else {
				a.setStatus("File saved as: " + filename)
				a.s.tabs[a.s.tabIdx] = filename // update current tab
				a.s.focus = focusEditor
			}
		default:
			a.setStatus("unknown command: " + cmd)
		}
	case ':':
		line, err := strconv.Atoi(cmd[1:])
		if err != nil {
			a.setStatus("Invalid line number")
			return
		}
		if line < 1 || line > a.s.lines.Len() {
			a.setStatus("Line number out of range")
			return
		}
		a.jump(line-1, 0)
		a.s.focus = focusEditor
		a.draw()
	case '@':
		symbolStr := cmd[1:]
		if symbolStr == "" {
			a.s.matchSymbols = nil
			a.setStatus("Usage: @symbol")
			return
		}
		var matched []Symbol
		for _, v := range a.s.symbols {
			for _, sym := range v {
				name := sym.Name
				if sym.Receiver != "" {
					name = sym.Receiver + "." + sym.Name
				}
				if strings.Contains(strings.ToLower(name), strings.ToLower(symbolStr)) {
					matched = append(matched, sym)
				}
			}
		}
		slices.SortFunc(matched, func(a, b Symbol) int {
			nameA := a.Name
			nameB := b.Name
			if a.Receiver != "" {
				nameA = a.Receiver + "." + a.Name
			}
			if b.Receiver != "" {
				nameB = b.Receiver + "." + b.Name
			}
			return strings.Compare(nameA, nameB)
		})
		a.s.matchSymbols = matched
		a.s.matchIdx = 0
		if len(matched) == 0 {
			a.setStatus("no matching symbols")
			return
		}
		ts := make([]textStyle, len(matched))
		for i, sym := range matched {
			text := sym.Name + " "
			if sym.Receiver != "" {
				text = sym.Receiver + "." + sym.Name + " "
			}
			if i == a.s.matchIdx {
				ts[i] = textStyle{text: text, style: highlightStyle}
			} else {
				ts[i] = textStyle{text: text}
			}
		}
		a.status.drawText(ts...)
	case '#':
		text := cmd[1:]
		if text == "" {
			a.setStatus("Usage: #text")
			return
		}
		row := a.s.row
		col := a.s.col
		var reverse bool
		start := a.s.line(row)
		for e := start; ; e = e.Next() {
			if e == nil {
				// reverse search
				e = a.s.lines.Front()
				row = 0
				col = 0
				reverse = true
			}
			if e == start && reverse {
				// reached the start again, no match found
				a.setConsole(cmd)
				a.syncCursor()
				return
			}
			line := e.Value.(string)
			if i := strings.Index(strings.ToLower(line[col:]), strings.ToLower(text)); i > -1 {
				a.s.row = row
				a.s.col = col + i + len(text)
				a.s.scroll = max(0, a.s.row-(len(a.editor)/2)) // viewport center
				// incremental search
				a.setConsole(cmd)
				a.draw()
				lineView := a.editor[a.s.row-a.s.scroll]
				// highlight the found text with tab expansion
				screenLine := expandTabs(line)
				screenStart := columnToScreen(line, col+i)
				screenEnd := columnToScreen(line, a.s.col)
				lineView.drawText(
					textStyle{text: screenLine[:screenStart]},
					textStyle{text: screenLine[screenStart:screenEnd], style: highlightStyle},
					textStyle{text: screenLine[screenEnd:]},
				)
				return
			}
			row++
			col = 0
		}
	default:
		// open file
		filename := cmd
		i := slices.Index(a.s.tabs, filename)
		if i < 0 {
			a.s.tabs = append(a.s.tabs, filename)
			i = len(a.s.tabs) - 1
		}
		a.s.switchTab(i)
		a.s.focus = focusEditor
		a.draw()
	}
}

var highlightStyle = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorLightYellow)

func (a *App) commandLoop() {
	for {
		select {
		case cmd := <-a.cmdCh:
			log.Printf("Command received: %q", cmd)
			a.handleCommand(cmd)
		case <-a.done:
			return
		}
	}
}

func (a *App) syncCursor() {
	switch a.s.focus {
	case focusEditor:
		if a.s.row < a.s.scroll || a.s.row > (a.s.scroll+len(a.editor)-1) {
			// out of viewport
			screen.HideCursor()
			return
		}
		// Convert cursor position from original line to screen position
		lineElement := a.s.line(a.s.row)
		if lineElement == nil {
			screen.HideCursor()
			return
		}
		line := lineElement.Value.(string)
		screenCol := columnToScreen(line, a.s.col)
		screen.ShowCursor(a.editor[0].x+screenCol, a.editor[0].y+a.s.row-a.s.scroll)
	case focusConsole:
		screen.ShowCursor(a.console.x+a.s.consoleCursor, a.console.y)
	default:
		screen.HideCursor()
	}
}

func (a *App) editorEvent(ev *tcell.EventKey) {
	defer func() {
		a.syncCursor()
		a.setStatus(fmt.Sprintf("Line %d, Column %d", a.s.row+1, a.s.col+1))
		if ev.Key() != tcell.KeyUp && ev.Key() != tcell.KeyDown {
			a.s.upDownCol = -1
		}
	}()
	switch ev.Key() {
	case tcell.KeyCtrlZ:
		a.s.undo()
		a.drawEditor()
	case tcell.KeyCtrlY:
		a.s.redo()
		a.drawEditor()
	case tcell.KeyRune:
		var line string
		if e := a.s.line(a.s.row); e == nil {
			line = string(ev.Rune())
			a.s.lines.PushBack(line)
		} else {
			line = e.Value.(string)
			line = line[:a.s.col] + string(ev.Rune()) + line[a.s.col:]
			e.Value = line
		}
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     a.s.col,
			newText: string(ev.Rune()),
			kind:    editInsert,
		})
		a.s.col++
		a.editor[a.s.row-a.s.scroll].drawEditorLine(line)
	case tcell.KeyEnter:
		a.s.lastEdit = nil
		a.s.undoStack = nil
		a.s.redoStack = nil
		if e := a.s.line(a.s.row); e == nil {
			// file end
			a.s.lines.PushBack("")
		} else {
			// break the line
			line := e.Value.(string)
			indent := indentation(line)
			a.s.lines.InsertAfter(indent+line[a.s.col:], e)
			e.Value = line[:a.s.col]
			a.s.col = len(indent)
		}
		a.s.row++
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
		}
		a.drawEditor()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		// backspace at line start, delete current line and move up
		if a.s.col == 0 {
			if a.s.row == 0 {
				return // no line to delete
			}

			a.s.lastEdit = nil
			a.s.undoStack = nil
			a.s.redoStack = nil
			line := a.s.line(a.s.row)
			prevLine := line.Prev()
			a.s.col = len(prevLine.Value.(string))
			prevLine.Value = prevLine.Value.(string) + line.Value.(string)
			a.s.lines.Remove(line)
			a.s.row--
			a.drawEditor()
			return
		}

		line := a.s.line(a.s.row)
		text := line.Value.(string)
		deleted := text[a.s.col-1]
		text = text[:a.s.col-1] + text[a.s.col:]
		line.Value = text
		a.s.recordEdit(Edit{
			row:     a.s.row,
			col:     a.s.col - 1,
			oldText: string(deleted),
			kind:    editDelete,
		})
		a.s.col--
		a.editor[a.s.row-a.s.scroll].drawEditorLine(text)
	case tcell.KeyLeft:
		a.s.lastEdit = nil
		// file start
		if a.s.row == 0 && a.s.col == 0 {
			return
		}
		if a.s.col == 0 {
			// move to previous line
			a.s.row--
			line := a.s.line(a.s.row).Value.(string)
			a.s.col = len(line)
			if a.s.row < a.s.scroll {
				a.s.scroll--
				a.drawEditor()
			}
			return
		}
		a.s.col--
	case tcell.KeyRight:
		a.s.lastEdit = nil
		lineItem := a.s.line(a.s.row)
		if lineItem == nil {
			return
		}

		line := lineItem.Value.(string)
		// middle of the line
		if a.s.col < len(line) {
			a.s.col++
			return
		}
		// file end
		if lineItem.Next() == nil {
			return
		}
		// line end, move to next line
		a.s.row++
		a.s.col = 0
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
			a.drawEditor()
		}
	case tcell.KeyUp:
		a.s.lastEdit = nil
		if a.s.row == 0 {
			return // already at the top
		}

		a.s.row--
		line := a.s.line(a.s.row).Value.(string)
		if line == "" {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = min(len(line), a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = min(len(line), a.s.col)
		}
		if a.s.row < a.s.scroll {
			a.s.scroll--
			a.drawEditor()
			return
		}
	case tcell.KeyDown:
		a.s.lastEdit = nil
		if a.s.row == a.s.lines.Len()-1 {
			return // already at the bottom
		}

		a.s.row++
		line := a.s.line(a.s.row).Value.(string)
		if line == "" {
			a.s.col = 0
		} else if a.s.upDownCol >= 0 {
			// moving up/down, keep previous column
			a.s.col = min(len(line), a.s.upDownCol)
		} else {
			// start moving up/down, keep current column
			a.s.upDownCol = a.s.col
			a.s.col = min(len(line), a.s.col)
		}
		if a.s.row >= a.s.scroll+len(a.editor) {
			a.s.scroll++
			a.drawEditor()
			return
		}
	case tcell.KeyCtrlA:
		a.s.lastEdit = nil
		a.s.col = 0
	case tcell.KeyCtrlE:
		a.s.lastEdit = nil
		e := a.s.line(a.s.row)
		if e == nil {
			return
		}
		a.s.col = len(e.Value.(string))
	case tcell.KeyTAB:
		a.s.recordEdit(Edit{row: a.s.row, col: a.s.col, newText: "\t", kind: editInsert})
		s := "\t"
		e := a.s.line(a.s.row)
		if e == nil {
			e = a.s.lines.PushBack(s)
		} else {
			line := e.Value.(string)
			line = line[:a.s.col] + s + line[a.s.col:]
			a.s.col += len(s)
			e.Value = line
		}
		a.editor[a.s.row-a.s.scroll].drawEditorLine(e.Value.(string))
	}
}

func (st *State) undo() {
	if len(st.undoStack) == 0 {
		return
	}

	e := st.undoStack[len(st.undoStack)-1]
	st.undoStack = st.undoStack[:len(st.undoStack)-1]
	st.apply(reverse(e))
	st.redoStack = append(st.redoStack, e)
}

func (st *State) redo() {
	if len(st.redoStack) == 0 {
		return
	}
	e := st.redoStack[len(st.redoStack)-1]
	st.redoStack = st.redoStack[:len(st.redoStack)-1]
	st.apply(e)
	st.undoStack = append(st.undoStack, e)
}

func reverse(e Edit) Edit {
	if e.kind == editInsert {
		return Edit{
			row:     e.row,
			col:     e.col,
			oldText: e.newText,
			kind:    editDelete,
		}
	}
	return Edit{
		row:     e.row,
		col:     e.col,
		newText: e.oldText,
		kind:    editInsert,
	}
}

func (st *State) apply(e Edit) {
	line := st.line(e.row)
	if line == nil {
		return
	}
	text := line.Value.(string)

	switch e.kind {
	case editInsert:
		text = text[:e.col] + e.newText + text[e.col:]
		st.col = e.col + len(e.newText)
	case editDelete:
		text = text[:e.col] + text[e.col+len(e.oldText):]
		st.col = e.col
	}
	line.Value = text
	st.row = e.row
}

// recordEdit adds an edit operation to the undo stack with intelligent coalescing.
// It merges consecutive edits of the same type that occur within 1 second on the same row
// to create more intuitive undo/redo behavior.
func (s *State) recordEdit(e Edit) {
	now := time.Now()

	if s.lastEdit != nil && e.kind == s.lastEdit.kind &&
		e.row == s.lastEdit.row && now.Sub(s.lastEdit.time) < time.Second {
		if e.kind == editInsert && s.lastEdit.col+len(s.lastEdit.newText) == e.col {
			s.lastEdit.newText += e.newText
			s.lastEdit.time = now
			return
		}

		if e.kind == editDelete && e.col == s.lastEdit.col-len(e.oldText) {
			s.lastEdit.oldText = e.oldText + s.lastEdit.oldText
			s.lastEdit.col = e.col
			s.lastEdit.time = now
			return
		}
	}

	e.time = now
	s.undoStack = append(s.undoStack, e)
	s.redoStack = nil
	s.lastEdit = &s.undoStack[len(s.undoStack)-1]
}

type SymbolKind string

const (
	SymbolFunc   SymbolKind = "func"
	SymbolType   SymbolKind = "type"
	SymbolVar    SymbolKind = "var"
	SymbolConst  SymbolKind = "const"
	SymbolImport SymbolKind = "import"
	SymbolField  SymbolKind = "field"
)

type Symbol struct {
	Name     string     // e.g., "Foo"
	Kind     SymbolKind // e.g., "func", "type"
	File     string     // absolute or relative path
	Line     int        // line number
	Column   int        // optional, for precision
	Receiver string     // for method: struct name, for field: struct name
}

func ParseSymbol(filename string) (map[string][]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		return nil, err
	}

	index := make(map[string][]Symbol)
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {

		case *ast.FuncDecl:
			pos := fset.Position(node.Pos())
			receiver := ""
			if node.Recv != nil && len(node.Recv.List) > 0 {
				typ := node.Recv.List[0].Type
				switch t := typ.(type) {
				case *ast.Ident:
					receiver = t.Name
				case *ast.StarExpr:
					if ident, ok := t.X.(*ast.Ident); ok {
						receiver = ident.Name
					}
				}
			}
			sym := Symbol{
				Name:     node.Name.Name,
				Kind:     SymbolFunc,
				File:     filename,
				Line:     pos.Line,
				Column:   pos.Column,
				Receiver: receiver,
			}
			index[sym.Name] = append(index[sym.Name], sym)

		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch ts := spec.(type) {
				case *ast.TypeSpec:
					pos := fset.Position(ts.Pos())
					sym := Symbol{
						Name:   ts.Name.Name,
						Kind:   SymbolType,
						File:   filename,
						Line:   pos.Line,
						Column: pos.Column,
					}
					index[sym.Name] = append(index[sym.Name], sym)

					// struct fields
					if structType, ok := ts.Type.(*ast.StructType); ok {
						for _, field := range structType.Fields.List {
							for _, name := range field.Names {
								fieldPos := fset.Position(name.Pos())
								fieldSym := Symbol{
									Name:     name.Name,
									Kind:     SymbolField,
									File:     filename,
									Line:     fieldPos.Line,
									Column:   fieldPos.Column,
									Receiver: ts.Name.Name,
								}
								index[fieldSym.Name] = append(index[fieldSym.Name], fieldSym)
							}
						}
					}

				case *ast.ValueSpec:
					for _, name := range ts.Names {
						pos := fset.Position(name.Pos())
						kind := SymbolVar
						if node.Tok == token.CONST {
							kind = SymbolConst
						}
						sym := Symbol{
							Name:   name.Name,
							Kind:   kind,
							File:   filename,
							Line:   pos.Line,
							Column: pos.Column,
						}
						index[sym.Name] = append(index[sym.Name], sym)
					}
				}
			}
		}
		return true
	})
	return index, nil
}
