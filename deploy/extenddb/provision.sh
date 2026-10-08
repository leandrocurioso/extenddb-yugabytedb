#!/bin/sh
# One-shot provisioning for a cell's ExtendDB instance: creates the IAM user the
# cell's Go app signs requests with and imports a fixed access key for it, so
# the app can be configured up front. APP_ACCESS_KEYS (space separated id:secret
# pairs) imports several keys for the same user instead of the single
# APP_ACCESS_KEY_ID/APP_SECRET_ACCESS_KEY. Idempotent: re-runs skip existing objects.
set -eu

CONFIG=/var/lib/extenddb/extenddb.toml
export EXTENDDB_PASSWORD="$EXTENDDB_ADMIN_PASSWORD"

manage() {
  extenddb manage --config "$CONFIG" --endpoint "$EXTENDDB_ENDPOINT" \
    --user "$EXTENDDB_ADMIN_USER" "$@"
}

ACCOUNT_ID=$(manage list-accounts | grep -oE '[0-9]{12}' | head -n 1)
if [ -z "$ACCOUNT_ID" ]; then
  echo "provision[$CELL_ID]: could not determine default account id" >&2
  exit 1
fi
echo "provision[$CELL_ID]: account $ACCOUNT_ID"

if manage list-users --account-id "$ACCOUNT_ID" | grep -q "\"$APP_USER\""; then
  echo "provision[$CELL_ID]: user $APP_USER already exists"
else
  manage create-user --account-id "$ACCOUNT_ID" --user-name "$APP_USER"
fi

manage put-user-policy --account-id "$ACCOUNT_ID" --user-name "$APP_USER" \
  --policy-name dynamodb-full-access \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"dynamodb:*","Resource":"*"}]}'

EXISTING_KEYS=$(manage list-access-keys --account-id "$ACCOUNT_ID" --user-name "$APP_USER")
for pair in ${APP_ACCESS_KEYS:-$APP_ACCESS_KEY_ID:$APP_SECRET_ACCESS_KEY}; do
  key_id=${pair%%:*}
  if echo "$EXISTING_KEYS" | grep -q "$key_id"; then
    echo "provision[$CELL_ID]: access key $key_id already imported"
  else
    manage import-access-key --account-id "$ACCOUNT_ID" --user-name "$APP_USER" \
      --access-key-id "$key_id" --secret-access-key "${pair#*:}" --yes
  fi
done

echo "provision[$CELL_ID]: done"
