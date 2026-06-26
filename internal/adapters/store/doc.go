// Package store implementa adapters de estado in-memory para o AI Provider Pool.
//
// MVP: única implementação concreta (Opção A, 1 réplica, sem Redis).
// Troca futura por RedisStore é cirúrgica — apenas adicionar uma nova
// implementação das interfaces em internal/domain (spec §3.1.3).
package store
