set -e
r=$(echo one; false; echo two)
echo "r=[$r]"
echo reached-end
