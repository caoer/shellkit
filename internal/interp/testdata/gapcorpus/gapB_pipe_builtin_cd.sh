cd "$HOME"
mkdir -p sub
echo y | builtin cd sub
pwd | sed "s#$HOME#HOME#"
