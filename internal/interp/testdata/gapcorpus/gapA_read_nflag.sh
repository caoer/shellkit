read -n 3 v < /dev/null
echo "${v:-empty}"
