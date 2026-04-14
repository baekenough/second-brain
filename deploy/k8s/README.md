# second-brain minikube deployment

## 1. Start minikube

```bash
minikube start --driver=docker --cpus=4 --memory=8g
```

## 2. Mount Google Drive folder

minikube 내부에서 호스트의 Google Drive 폴더를 `/mnt/drive`로 마운트합니다.
백그라운드로 실행 후 터미널을 열어둬야 합니다.

```bash
minikube mount "/Users/sangyi/Google Drive/공유 드라이브/Vibers.AI:/mnt/drive" &
```

## 3. Build image in minikube's Docker

minikube 내장 Docker 데몬을 사용해 이미지를 빌드합니다 (registry push 불필요).

```bash
eval $(minikube docker-env)
docker build -t second-brain:dev .
```

## 4. Configure secrets

`second-brain-secret.yaml`의 `CHANGE_ME` 값을 실제 값으로 교체하거나,
kubectl로 직접 생성합니다:

```bash
kubectl create secret generic second-brain-secret \
  --from-literal=DATABASE_URL="postgres://brain:brain@postgres:5432/second_brain?sslmode=disable" \
  --from-literal=POSTGRES_PASSWORD="brain" \
  --from-literal=API_KEY="<실제 키>" \
  --from-literal=EMBEDDING_API_KEY="<실제 키>" \
  -n second-brain
```

## 5. Apply manifests

```bash
kubectl apply -k deploy/k8s/
```

## 6. Verify

```bash
# Pod 상태 확인
kubectl -n second-brain get pods

# 로그 확인
kubectl -n second-brain logs -l app=second-brain --tail=50

# Postgres 로그
kubectl -n second-brain logs -l app=postgres --tail=50
```

## 7. Access

```bash
# NodePort URL (minikube IP:30920)
minikube service -n second-brain second-brain --url

# 또는 port-forward
kubectl -n second-brain port-forward svc/second-brain 9200:9200
# → http://localhost:9200
```

## 8. Update deployment

이미지 재빌드 후 롤아웃:

```bash
eval $(minikube docker-env)
docker build -t second-brain:dev .
kubectl -n second-brain rollout restart deployment/second-brain
kubectl -n second-brain rollout status deployment/second-brain
```

## 9. Teardown

```bash
kubectl delete -k deploy/k8s/
minikube stop
```

## Notes

- `second-brain-pv.yaml`은 minikube hostPath PV입니다. `minikube mount` 세션이 살아있어야 `/mnt/drive`가 채워집니다.
- 실제 운영 환경에서는 sealed-secrets 또는 외부 secrets manager를 사용하세요.
- `imagePullPolicy: IfNotPresent`이므로 이미지 재빌드 후 반드시 rollout restart 필요.
