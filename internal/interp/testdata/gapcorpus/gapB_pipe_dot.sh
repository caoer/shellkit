printf 'MARK=1\n' > y.sh
echo y | . y.sh
echo "MARK=${MARK-none}"
