# Seyi Text

A mini text editor around 2k lines of code.

Usage:
```
go build -o seyi .
./seyi [file]
```

Key bindings:
```
ctrl-o open file
ctrl-s save file
ctrl-q quit
ctrl-t new tab
ctrl-w close tab
ctrl-f find
ctrl-c copy
ctrl-v paste
ctrl-z undo
ctrl-y redo
ctrl-_ go back
ctrl-g go to line
ctrl-r go to symbol
ctrl-a go to line start
ctrl-e go to line end
ctrl-u delete back to line start
ctrl-p command
shift-tab decrease indent
```

Console commands:
- `#<text>` find text
- `@<symbol>` go to symbol
- `:<line>` go to line
- `>open <file>`
- `>save <file>`
- `>linenumber` toggle line number
- `>back` go back
- `>forward` go forward
