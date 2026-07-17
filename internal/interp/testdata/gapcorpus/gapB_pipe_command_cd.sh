cd "$HOME"
mkdir -p sub
echo x | command cd sub
pwd | sed "s#$HOME#HOME#"
