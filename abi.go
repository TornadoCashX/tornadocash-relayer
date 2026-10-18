package main

// tornadoPoolABI covers the Tornado Cash instance (pool) contract methods used
// by the relayer. Withdrawals are sent directly to the pool contract, e.g.
// mainnet "Tornado.Cash: 10 ETH" 0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF,
// instead of going through the Tornado Router.
const tornadoPoolABI = `[
  {
    "constant": false,
    "inputs": [
      { "internalType": "bytes", "name": "_proof", "type": "bytes" },
      { "internalType": "bytes32", "name": "_root", "type": "bytes32" },
      { "internalType": "bytes32", "name": "_nullifierHash", "type": "bytes32" },
      { "internalType": "address payable", "name": "_recipient", "type": "address" },
      { "internalType": "address payable", "name": "_relayer", "type": "address" },
      { "internalType": "uint256", "name": "_fee", "type": "uint256" },
      { "internalType": "uint256", "name": "_refund", "type": "uint256" }
    ],
    "name": "withdraw",
    "outputs": [],
    "payable": true,
    "stateMutability": "payable",
    "type": "function"
  },
  {
    "constant": true,
    "inputs": [{ "name": "_root", "type": "bytes32" }],
    "name": "isKnownRoot",
    "outputs": [{ "name": "", "type": "bool" }],
    "payable": false,
    "stateMutability": "view",
    "type": "function"
  },
  {
    "constant": true,
    "inputs": [{ "name": "_nullifierHash", "type": "bytes32" }],
    "name": "isSpent",
    "outputs": [{ "name": "", "type": "bool" }],
    "payable": false,
    "stateMutability": "view",
    "type": "function"
  },
  {
    "constant": true,
    "inputs": [],
    "name": "denomination",
    "outputs": [{ "internalType": "uint256", "name": "", "type": "uint256" }],
    "payable": false,
    "stateMutability": "view",
    "type": "function"
  },
  {
    "constant": true,
    "inputs": [],
    "name": "token",
    "outputs": [{ "internalType": "contract IERC20", "name": "", "type": "address" }],
    "payable": false,
    "stateMutability": "view",
    "type": "function"
  }
]`

const offchainOracleABI = `[
  {
    "inputs": [
      { "internalType": "contract IERC20", "name": "srcToken", "type": "address" },
      { "internalType": "bool", "name": "useSrcWrappers", "type": "bool" }
    ],
    "name": "getRateToEth",
    "outputs": [{ "internalType": "uint256", "name": "weightedRate", "type": "uint256" }],
    "stateMutability": "view",
    "type": "function"
  }
]`
