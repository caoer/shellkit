V=1
echo x | unset V
echo "V=${V-UNSET}"
