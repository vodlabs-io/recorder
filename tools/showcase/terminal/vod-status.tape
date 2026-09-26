# `vod status` against the Demo daemon: a few seconds of the live view as
# recording stats climb. Run through render-terminal.sh, which starts the
# daemon and sets VOD_BIN and VOD_CONFIG.

Output vod-status.gif
Output vod-status.mp4

Set Shell "bash"
Set FontSize 18
Set Width 1180
Set Height 330
Set Padding 28
Set Margin 0
Set WindowBar Colorful
Set BorderRadius 12
Set TypingSpeed 70ms
Set Theme { "name": "vodlabs", "black": "#1b1b1b", "red": "#ed6765", "green": "#65c366", "yellow": "#f3c944", "blue": "#6ea8fe", "magenta": "#c792ea", "cyan": "#5fd7d7", "white": "#d0d0d0", "brightBlack": "#6c6c6c", "brightRed": "#ff8a87", "brightGreen": "#8be28c", "brightYellow": "#ffe083", "brightBlue": "#9cc4ff", "brightMagenta": "#e2b3ff", "brightCyan": "#8ff0f0", "brightWhite": "#ffffff", "background": "#0f0f10", "foreground": "#e6e6e6", "selection": "#333333", "cursor": "#e6e6e6" }

Hide
Type 'export PS1="\[\e[38;5;244m\]~\[\e[0m\] $ "; vod() { "$VOD_BIN" --config "$VOD_CONFIG" "$@"; }; clear'
Enter
Show

Sleep 600ms
Type "vod status"
Sleep 400ms
Enter
Sleep 7s
