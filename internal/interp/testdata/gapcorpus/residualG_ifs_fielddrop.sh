data=":a:b"
IFS=:
set -- $data
echo "count=$#"
