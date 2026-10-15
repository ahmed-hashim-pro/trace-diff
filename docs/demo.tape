# Records docs/demo.gif: `make gif`.
Output docs/demo.gif
Set Shell bash
Set FontSize 13
Set Width 1200
Set Height 860
Set Padding 16
Set TypingSpeed 25ms
Set Theme "GitHub Dark"

Hide
Type "export PATH=$PWD:$PATH S=examples/shop && clear" Enter
Show

Type "# the same code, run twice" Enter
Type "trace-diff check $S/before.jsonl.gz $S/rerun.jsonl.gz" Enter
Sleep 2.5s
Type "clear" Enter
Type "# the pull request: lazy-loaded items, and retries on pricing" Enter
Type "trace-diff check $S/before.jsonl.gz $S/after.jsonl.gz; echo exit $?" Enter
Sleep 9s
