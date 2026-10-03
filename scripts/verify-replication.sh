#!/usr/bin/env bash
# Vérifie l'état de la réplication et teste la propagation d'une écriture.
set -euo pipefail

NAMESPACE="mysql-repl"
ROOT_PASSWORD="rootpass123"
MASTER_POD="mysql-master-0"
SLAVE_POD="mysql-slave-0"

echo "=== Statut réplica (slave) ==="
kubectl -n "${NAMESPACE}" exec -i "${SLAVE_POD}" -- \
  mysql -uroot -p"${ROOT_PASSWORD}" -e "SHOW REPLICA STATUS\G" | \
  grep -E "Source_Host|Replica_IO_Running|Replica_SQL_Running|Seconds_Behind_Source|Last_Error"

echo
echo "=== Test fonctionnel : écriture sur master, lecture sur slave ==="
STAMP=$(date +%s)
kubectl -n "${NAMESPACE}" exec -i "${MASTER_POD}" -- \
  mysql -uroot -p"${ROOT_PASSWORD}" -e "
    CREATE DATABASE IF NOT EXISTS repl_test;
    CREATE TABLE IF NOT EXISTS repl_test.ping (id INT PRIMARY KEY, ts BIGINT);
    REPLACE INTO repl_test.ping (id, ts) VALUES (1, ${STAMP});
  "

echo "Attente propagation (3s)..."
sleep 3

RESULT=$(kubectl -n "${NAMESPACE}" exec -i "${SLAVE_POD}" -- \
  mysql -uroot -p"${ROOT_PASSWORD}" -N -e "SELECT ts FROM repl_test.ping WHERE id=1;")

if [ "${RESULT}" = "${STAMP}" ]; then
  echo "OK : valeur ${STAMP} répliquée correctement sur le slave."
else
  echo "ECHEC : valeur attendue ${STAMP}, obtenue '${RESULT}'."
  exit 1
fi
