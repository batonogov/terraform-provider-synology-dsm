# The resource is a singleton, so the ID is a fixed placeholder: DSM has exactly
# one set of date and time settings and the whole state is read back from it.
terraform import dsm_system_settings.this system_settings
