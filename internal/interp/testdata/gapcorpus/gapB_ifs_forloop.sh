data="a b c"
IFS=,
for x in $data; do echo "[$x]"; done
