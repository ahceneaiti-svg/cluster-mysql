#!/usr/bin/env bash
# Configure la réplication maître -> esclave via GTID auto-position.
set -euo pipefail

NAMESPACE="mysql-repl"
ROOT_PASSWORD="rootpass123"
REPL_USER="repl_user"
REPL_PASSWORD="replpass123"
MASTER_POD="mysql-master-0"
SLAVE_POD="mysql-slave-0"
MASTER_HOST="mysql-master.${NAMESPACE}.svc.cluster.local"

echo "Attente que les pods soient prêts..."
kubectl -n "${NAMESPACE}" wait --for=condition=Ready pod/${MASTER_POD} --timeout=180s
kubectl -n "${NAMESPACE}" wait --for=condition=Ready pod/${SLAVE_POD} --timeout=180s

echo "Configuration de la réplication sur l'esclave..."
kubectl -n "${NAMESPACE}" exec -i "${SLAVE_POD}" -- \
  mysql -uroot -p"${ROOT_PASSWORD}" -e "
    STOP REPLICA;
    CHANGE REPLICATION SOURCE TO
      SOURCE_HOST='${MASTER_HOST}',
      SOURCE_PORT=3306,
      SOURCE_USER='${REPL_USER}',
      SOURCE_PASSWORD='${REPL_PASSWORD}',
      SOURCE_AUTO_POSITION=1;
    START REPLICA;
    SET GLOBAL read_only=ON;
    SET GLOBAL super_read_only=ON;
  "

echo "Réplication démarrée. Statut :"
kubectl -n "${NAMESPACE}" exec -i "${SLAVE_POD}" -- \
  mysql -uroot -p"${ROOT_PASSWORD}" -e "SHOW REPLICA STATUS\G" | \
  grep -E "Source_Host|Replica_IO_Running|Replica_SQL_Running|Last_Error"
