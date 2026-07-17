printf 'a\nb\n' | mapfile -t A
echo "len=${#A[@]}"
