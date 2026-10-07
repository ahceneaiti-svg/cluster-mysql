# MySQL Master/Slave sur kind

Cluster MySQL 8.0 avec réplication maître-esclave (GTID), déployé via manifests Kubernetes bruts sur kind (Kubernetes local).

Voir [INSTALL.md](INSTALL.md) pour le déploiement complet, l'activation de la réplication et la vérification.

## Résumé

- `k8s/` — namespace, secret, configmaps (master/slave), StatefulSets + Services
- `scripts/setup-replication.sh` — active la réplication (GTID auto-position)
- `scripts/verify-replication.sh` — vérifie le statut + teste la propagation d'une écriture
- `operator/` — opérateur Backup/Restore (CRDs `MySQLBackup`, `MySQLRestore`, `MySQLBackupSchedule`), voir [operator/README.md](operator/README.md)
- `kind-cluster.yaml` — config d'un cluster kind dédié (optionnel)

Statut validé sur ce poste : réplication active, `Replica_IO_Running` / `Replica_SQL_Running` = `Yes`, écriture sur le master répliquée sur le slave en lecture seule (`super_read_only=1`).
