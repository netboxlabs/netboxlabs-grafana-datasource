import os

from netbox_branching.utilities import DynamicSchemaDict

# netbox_branching must be the last entry if more plugins are added later.
PLUGINS = ["netbox_branching"]

# netbox-branching requires DATABASES (plural) wrapped in DynamicSchemaDict plus
# a branch-aware router. Read the same DB_* env the demo already sets so this
# never drifts from the compose Postgres.
DATABASES = DynamicSchemaDict(
    {
        "default": {
            "ENGINE": "django.db.backends.postgresql",
            "NAME": os.environ.get("DB_NAME", "netbox"),
            "USER": os.environ.get("DB_USER", "netbox"),
            "PASSWORD": os.environ.get("DB_PASSWORD", ""),
            "HOST": os.environ.get("DB_HOST", "postgres"),
            "PORT": os.environ.get("DB_PORT", ""),
            "CONN_MAX_AGE": 300,
        }
    }
)

DATABASE_ROUTERS = ["netbox_branching.database.BranchAwareRouter"]
