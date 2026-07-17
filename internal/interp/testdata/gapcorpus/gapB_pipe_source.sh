printf 'MARK=1\n' > x.sh
echo y | source x.sh
echo "MARK=${MARK-none}"
