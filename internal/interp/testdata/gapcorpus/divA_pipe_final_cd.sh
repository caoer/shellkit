cd "$HOME"
mkdir -p sub
echo x | cd sub
pwd | sed "s#$HOME#HOME#"
