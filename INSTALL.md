# Installation — Cluster MySQL Master/Slave sur kind

Documentation technique pour déployer un cluster MySQL 8.0 avec réplication maître-esclave (GTID) sur un cluster Kubernetes local (kind), activer la réplication et vérifier son bon fonctionnement.

## Prérequis

- `docker` (daemon actif)
- `kind` (testé avec v0.32.0)
- `kubectl` (testé avec v1.36.1)
- Au moins ~1 Gi de RAM libre pour les deux pods MySQL (mémoire limitée observée en pratique : si un cluster kind dédié échoue à démarrer avec `could not find a log line that matches "Reached target .*Multi-User System.*"`, c'est généralement un manque de RAM — réutiliser un cluster kind existant plutôt que d'en créer un nouveau)

## Architecture

```
mysql/
├── kind-cluster.yaml          # config cluster kind dédié (optionnel, voir note ressources)
├── k8s/
│   ├── 00-namespace.yaml       # namespace mysql-repl
│   ├── 01-secret.yaml          # credentials root + utilisateur de réplication
│   ├── 02-configmap-master.yaml # my.cnf master (server-id=1, binlog, GTID) + script init SQL
│   ├── 03-configmap-slave.yaml  # my.cnf slave (server-id=2, binlog, GTID)
│   ├── 04-master.yaml          # Service headless + StatefulSet master
│   └── 05-slave.yaml           # Service headless + StatefulSet slave
├── operator/                   # opérateur Backup/Restore (Go) — voir section 5
├── scripts/
│   ├── setup-replication.sh    # configure CHANGE REPLICATION SOURCE + START REPLICA
│   └── verify-replication.sh   # vérifie statut + test fonctionnel d'écriture/lecture
└── INSTALL.md
```

Chaque rôle (master/slave) est un `StatefulSet` à 1 réplica avec son propre `PersistentVolumeClaim` (500Mi) et son `Service` headless, exposés en interne au cluster via DNS (`mysql-master.mysql-repl.svc.cluster.local`).

## 1. Préparer le cluster kind

### Option A — cluster dédié (recommandé si assez de RAM disponible)

```bash
kind create cluster --config kind-cluster.yaml
kubectl config use-context kind-mysql-cluster
```

### Option B — réutiliser un cluster kind existant

Si la création d'un nouveau nœud kind échoue (RAM insuffisante, erreur `Preparing nodes` / `Reached target Multi-User System`), réutiliser un cluster kind déjà actif :

```bash
kind get clusters
kubectl config use-context kind-<nom-du-cluster-existant>
```

Le namespace `mysql-repl` isole les ressources, donc cette option ne touche pas aux autres workloads du cluster.

## 2. Déployer les manifests

```bash
kubectl apply -f k8s/00-namespace.yaml
kubectl apply -f k8s/01-secret.yaml
kubectl apply -f k8s/02-configmap-master.yaml
kubectl apply -f k8s/03-configmap-slave.yaml
kubectl apply -f k8s/04-master.yaml
kubectl apply -f k8s/05-slave.yaml
```

Ou en une commande :

```bash
kubectl apply -f k8s/
```

Attendre que les deux pods soient prêts (téléchargement image `mysql:8.0` inclus, peut prendre 1-3 min) :

```bash
kubectl -n mysql-repl wait --for=condition=Ready pod/mysql-master-0 --timeout=300s
kubectl -n mysql-repl wait --for=condition=Ready pod/mysql-slave-0 --timeout=300s
kubectl -n mysql-repl get pods
```

À ce stade :
- Le master a déjà créé l'utilisateur de réplication (`repl_user`) via le script d'init SQL monté dans `/docker-entrypoint-initdb.d/`.
- Le slave démarre **sans** `read_only`/`super_read_only` dans sa config initiale — ces valeurs sont activées après coup par le script de réplication (voir note ci-dessous).

> **Pourquoi pas `read_only`/`super_read_only` dans `my.cnf` du slave dès le départ ?**
> L'entrypoint officiel de l'image `mysql:8.0` exécute un `ALTER USER` pour sécuriser le compte root pendant l'initialisation. Si `super_read_only=ON` est déjà actif à ce moment, cette commande échoue (`ERROR 1290 ... --super-read-only`) et le mot de passe root n'est jamais positionné correctement. Ces options sont donc activées à la fin de `setup-replication.sh`, une fois le serveur pleinement initialisé.

## 3. Activer la réplication

```bash
bash scripts/setup-replication.sh
```

Ce script :
1. Attend que les deux pods soient `Ready`.
2. Exécute sur le slave :
   ```sql
   CHANGE REPLICATION SOURCE TO
     SOURCE_HOST='mysql-master.mysql-repl.svc.cluster.local',
     SOURCE_PORT=3306,
     SOURCE_USER='repl_user',
     SOURCE_PASSWORD='replpass123',
     SOURCE_AUTO_POSITION=1;
   START REPLICA;
   SET GLOBAL read_only=ON;
   SET GLOBAL super_read_only=ON;
   ```
3. Affiche le statut (`Source_Host`, `Replica_IO_Running`, `Replica_SQL_Running`, `Last_Error`).

La réplication utilise le positionnement automatique par GTID (`SOURCE_AUTO_POSITION=1`), pas de coordonnées binlog manuelles à relever.

## 4. Vérifier que la réplication fonctionne

```bash
bash scripts/verify-replication.sh
```

Ce script :
1. Affiche `SHOW REPLICA STATUS` filtré (IO/SQL running, retard en secondes, dernière erreur).
2. Fait un test fonctionnel : écrit une ligne horodatée dans `repl_test.ping` sur le **master**, attend 3s, relit la même ligne sur le **slave**, et compare.

Résultat attendu :

```
Replica_IO_Running: Yes
Replica_SQL_Running: Yes
Seconds_Behind_Source: 0
...
OK : valeur <timestamp> répliquée correctement sur le slave.
```

### Vérifications manuelles complémentaires

```bash
# Confirmer le slave est en lecture seule
kubectl -n mysql-repl exec -i mysql-slave-0 -- mysql -uroot -prootpass123 -e "SELECT @@read_only, @@super_read_only;"

# Statut complet du replica
kubectl -n mysql-repl exec -i mysql-slave-0 -- mysql -uroot -prootpass123 -e "SHOW REPLICA STATUS\G"

# Confirmer qu'une écriture directe est refusée sur le slave (read-only)
kubectl -n mysql-repl exec -i mysql-slave-0 -- mysql -uroot -prootpass123 -e "CREATE DATABASE test_write;"
# -> doit renvoyer une erreur de type "--read-only"
```

## 5. Backup / Restore avec l'opérateur

L'opérateur (`operator/`, détails dans [operator/README.md](operator/README.md)) gère sauvegardes et restaurations via trois CRDs : `MySQLBackup` (`mbk`), `MySQLRestore` (`mrs`) et `MySQLBackupSchedule` (`mbs`).

### 5.1 Déployer l'opérateur

Prérequis supplémentaire : `go` ≥ 1.26 (uniquement pour `make generate`/`make build`, pas pour le déploiement).

```bash
cd operator
make kind-load KIND_CLUSTER=<nom-du-cluster-kind>   # build + chargement de mysql-operator:dev
make deploy                                         # CRDs + RBAC + Deployment
kubectl -n mysql-operator-system get pods           # attendre Running
```

L'opérateur tourne dans le namespace `mysql-operator-system` et surveille tous les namespaces.

### 5.2 Sauvegarder

```bash
kubectl apply -f operator/config/samples/backup.yaml
kubectl -n mysql-repl get mbk          # PHASE: Pending -> Running -> Completed
```

- Le dump est pris sur le **slave** (pas de charge sur le master), avec `--single-transaction --routines --triggers --events --set-gtid-purged=OFF`.
- Fichier : `/<namespace>/<nom>.sql.gz` dans le PVC `mysql-backups` (créé automatiquement, 1Gi par défaut).
- `status` contient `file`, `sizeBytes` et `sha256`.
- Sans `spec.databases`, toutes les bases hors `information_schema`, `performance_schema`, `mysql`, `sys` sont sauvegardées (les comptes et privilèges ne le sont pas).

### 5.3 Restaurer

```bash
kubectl apply -f operator/config/samples/restore.yaml
kubectl -n mysql-repl get mrs          # PHASE: Running -> Completed
bash scripts/verify-replication.sh     # contrôler que la réplication est toujours saine
```

- La restauration cible le **master** ; le slave (`super_read_only`) reçoit les données par réplication.
- Le Job vérifie le SHA-256 et l'intégrité gzip avant d'appliquer le dump.
- Le `MySQLBackup` référencé doit être `Completed` : en attente s'il est en cours, échec s'il est `Failed` ou absent.
- Restauration partielle : `spec.databases: [appdb]`.
- Attention : les tables du dump sont recréées (`DROP TABLE` puis `CREATE`) ; les écritures postérieures au backup sur ces tables sont perdues.

### 5.4 Planifier

```bash
kubectl apply -f operator/config/samples/schedule.yaml   # tous les jours à 02:00, 7 backups conservés
kubectl -n mysql-repl get mbs
```

Les backups créés portent le nom `<schedule>-<AAAAMMJJ-HHMMSS>`. Au-delà de `keepLast`, les plus anciens sont supprimés avec leur fichier. `spec.suspend: true` met la planification en pause.

### 5.5 Supprimer un backup

```bash
kubectl -n mysql-repl delete mbk demo-backup
```

Un finalizer lance un Job `cleanup-<nom>` qui supprime le fichier du PVC avant de libérer l'objet.

### 5.6 Désinstaller l'opérateur

```bash
cd operator && make undeploy
kubectl delete -f operator/config/crd   # supprime aussi tous les MySQLBackup/Restore/Schedule
```

> Supprimer les CRDs avant les `MySQLBackup` bloque leurs finalizers : supprimer d'abord les backups (`kubectl delete mbk --all -A`), puis l'opérateur, puis les CRDs.

## Identifiants (démo locale uniquement)

| Usage | Valeur |
|---|---|
| Root password | `rootpass123` |
| Utilisateur réplication | `repl_user` |
| Mot de passe réplication | `replpass123` |

Définis dans `k8s/01-secret.yaml` (Secret Kubernetes) et dans le SQL d'init du master. **Ne pas réutiliser en production** — pour un déploiement réel, générer des mots de passe forts et les injecter via un gestionnaire de secrets externe (Vault, Sealed Secrets, etc.), ne jamais les committer en clair.

## Nettoyage

```bash
kubectl delete -f k8s/
# si cluster kind dédié créé pour l'occasion :
kind delete cluster --name mysql-cluster
```

## Dépannage

| Symptôme | Cause probable | Action |
|---|---|---|
| `kind create cluster` échoue sur `Preparing nodes` | RAM insuffisante pour booter un nouveau nœud systemd | Réutiliser un cluster kind existant (`kind get clusters`) |
| `ERROR 1045 Access denied for user 'root'` juste après déploiement | `read_only`/`super_read_only` actifs dès l'init, bloquant l'`ALTER USER` interne de l'entrypoint | Ne pas activer ces options dans `my.cnf` initial ; les activer après `START REPLICA` (déjà fait dans `setup-replication.sh`) |
| PVC bloqué en `Terminating` après suppression d'un pod | Le `StatefulSet` recrée le pod avant la suppression du PVC, qui reste attaché | `kubectl scale statefulset <nom> --replicas=0`, puis supprimer le PVC, puis remonter à `--replicas=1` |
| `MySQLBackup` en `Failed` | Source injoignable, mauvais Secret/mot de passe, ou aucune base utilisateur | `kubectl -n mysql-repl get mbk <nom> -o jsonpath='{.status.message}'` et `kubectl -n mysql-repl logs job/backup-<nom>` |
| `MySQLRestore` en `Failed` : `sha256sum: WARNING ... did NOT match` | Dump corrompu ou modifié dans le PVC | Refaire un backup ; ne pas restaurer ce fichier |
| `MySQLBackup` bloqué en `Terminating` | Le Job `cleanup-<nom>` n'aboutit pas (PVC absent/occupé) ou opérateur arrêté | Vérifier `kubectl -n mysql-repl get jobs` et l'opérateur ; en dernier recours retirer le finalizer `mysql.aia.local/cleanup` |
| Pod de l'opérateur en `ImagePullBackOff` | Image `mysql-operator:dev` absente du nœud kind | `make kind-load KIND_CLUSTER=<nom>` |
| `Replica_IO_Running: No` | Mauvais host/port/credentials, ou règle réseau bloquant le port 3306 | Vérifier `SHOW REPLICA STATUS\G` champ `Last_IO_Error`, et la résolution DNS du Service master depuis le pod slave |
