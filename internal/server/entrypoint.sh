#!/bin/sh
set -e

# Run Atlas schema migration if a database URL is configured.
if [ -n "$AILERON_DATABASE_URL" ]; then
  echo "Applying database schema..."
  atlas schema apply \
    --url "$AILERON_DATABASE_URL" \
    --to "file:///schema/schema.hcl" \
    --auto-approve
  echo "Schema applied successfully."
fi

exec aileron-server
