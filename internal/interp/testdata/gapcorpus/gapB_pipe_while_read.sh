n=0
seq 3 | while read x; do n=$((n+1)); done
echo "n=$n"
