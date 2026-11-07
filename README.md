# MBA — Desafio de Arquitetura: Cotação de Seguros

PoC em Go para aumentar a resiliência da `quotation-api` por meio de timeout, circuit breaker, cache Redis e fallback parcial.

## Pré-requisitos

- Go 1.25
- Docker e Docker Compose v2
- GNU Make

## Como subir o ambiente

Na raiz do projeto, execute:

```bash
make up
```

Para verificar os serviços:

```bash
make ps
```

## Teste de carga

Com o ambiente em execução:

```bash
make load
```

## Testes automatizados

```bash
make test
```

## Documentação

- [Enunciado do desafio](docs/enunciado.md)
- [System Architecture Document (SAD)](docs/sad.md)
