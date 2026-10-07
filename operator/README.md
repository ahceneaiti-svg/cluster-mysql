# mysql-operator — Backup / Restore

Opérateur Kubernetes (Go, controller-runtime) qui gère les sauvegardes et restaurations du cluster MySQL master/slave de ce dépôt.

## CRDs (`mysql.aia.local/v1alpha1`)

| Kind | Alias | Rôle |
|------|-------|------|
| `MySQLBackup` | `mbk` | Backup logique ponctuel (`mysqldump`, gzip) vers un PVC |
| `MySQLRestore` | `mrs` | Restaure un `MySQLBackup` sur un serveur cible |
| `MySQLBackupSchedule` | `mbs` | Crée des `MySQLBackup` selon un cron + rétention `keepLast` |

## Fonctionnement

- **Backup** : Job `backup-<nom>` (image `mysql:8.0`) qui lance `mysqldump --single-transaction --routines --triggers --events --set-gtid-purged=OFF` sur la source (le **slave** par défaut dans les exemples, pour ne pas charger le master). Écriture dans `<claimName>:/<namespace>/<nom>.sql.gz.partial` puis renommage ; taille et SHA-256 remontés dans `status`.
- **Restore** : Job `restore-<nom>` qui vérifie le SHA-256, `gzip -t`, puis rejoue le dump sur la cible. Cible = **master** : le slave (`super_read_only`) est alimenté par la réplication. `--set-gtid-purged=OFF` évite le conflit de `gtid_executed`. Attend que le backup soit `Completed` ; échoue s'il est `Failed` ou absent.
- **PVC** `mysql-backups` (1Gi par défaut) créé automatiquement s'il n'existe pas.
- **Suppression d'un `MySQLBackup`** : un finalizer lance un Job qui supprime le fichier du PVC. La rétention du schedule s'appuie dessus.
- Sans `databases`, toutes les bases hors `information_schema`, `performance_schema`, `mysql`, `sys` sont traitées (comptes et privilèges ne sont donc pas sauvegardés).
- Mot de passe lu dans le Secret `mysql-secret` (clé `MYSQL_ROOT_PASSWORD`), configurable via `secretName` / `passwordKey` / `user`.

## Déploiement

Prérequis : docker, kind, kubectl, go ≥ 1.26.

```bash
cd operator
make kind-load KIND_CLUSTER=<cluster kind>   # build + chargement de l'image mysql-operator:dev
make deploy                                   # CRDs + RBAC + Deployment (ns mysql-operator-system)
kubectl -n mysql-operator-system get pods
```

## Utilisation

```bash
kubectl apply -f config/samples/backup.yaml
kubectl -n mysql-repl get mbk                  # PHASE / FILE / SIZE

kubectl apply -f config/samples/restore.yaml   # restaure sur le master
kubectl -n mysql-repl get mrs

kubectl apply -f config/samples/schedule.yaml  # backup quotidien 02:00, 7 conservés
```

Restauration partielle : `spec.databases: [appdb]`. Après un restore, vérifier la réplication avec `scripts/verify-replication.sh`.

## Développement

```bash
make tools      # installe controller-gen dans bin/
make generate   # deepcopy + CRDs + RBAC (après modif de api/ ou des marqueurs +kubebuilder:rbac)
make build
```

## Limites connues

- Backup logique : adapté à de petits volumes. Pour de gros jeux de données, passer à un backup physique (Percona XtraBackup) et/ou du stockage objet (S3).
- Stockage = PVC `ReadWriteOnce` local au cluster ; pas de copie hors cluster.
- Pas de PITR (binlog) : restauration au point du dump uniquement.
- Restore sur le master sans `databases` rejoue `DROP TABLE`/`CREATE` sur les tables du dump : les écritures postérieures au backup sur ces tables sont perdues.
