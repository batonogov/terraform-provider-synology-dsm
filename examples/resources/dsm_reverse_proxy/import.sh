# Reverse proxy entries are identified by the UUID DSM assigns on creation.
# Find it with:
#   curl -s "$SYNOLOGY_DSM_HOST/webapi/entry.cgi?_sid=$SID" \
#     -d api=SYNO.Core.AppPortal.ReverseProxy -d version=1 -d method=list
terraform import dsm_reverse_proxy.nextcloud 1b0d0c30-9e1f-4a2b-8f7e-2c9d1a5b7e40

# The entry description is accepted too; the provider normalizes it to the UUID.
terraform import dsm_reverse_proxy.nextcloud Nextcloud
