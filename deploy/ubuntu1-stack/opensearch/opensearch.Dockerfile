# OpenSearch + 한국어 형태소 분석기(nori)
#
# 현재 PostgreSQL FTS 는 pg_bigm(bigram) 을 쓴다. bigram 은 재현율이 높은 대신
# "회의"가 "사회의"에 걸리는 식의 오탐이 생긴다. nori 는 형태소 단위로 끊어
# 정밀도를 올린다 — OpenSearch 도입의 실질적 근거가 이것이므로 플러그인을
# 이미지에 미리 넣어 둔다(런타임 설치는 재기동마다 사라진다).
FROM opensearchproject/opensearch:2.19.1
RUN /usr/share/opensearch/bin/opensearch-plugin install --batch analysis-nori
