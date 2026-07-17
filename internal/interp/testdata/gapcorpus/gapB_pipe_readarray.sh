printf 'a\nb\n' | readarray -t A
echo "len=${#A[@]}"
