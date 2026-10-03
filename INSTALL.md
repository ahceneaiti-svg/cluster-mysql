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
| `Replica_IO_Running: No` | Mauvais host/port/credentials, ou règle réseau bloquant le port 3306 | Vérifier `SHOW REPLICA STATUS\G` champ `Last_IO_Error`, et la résolution DNS du Service master depuis le pod slave |
