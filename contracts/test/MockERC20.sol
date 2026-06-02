// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Minimal ERC-20 for tests. Tracks balances + allowances, supports a
///         `transferFromFails` switch to simulate USDC blocklist / insufficient
///         allowance reverts surfaced as a `false` return.
contract MockERC20 {
    string  public name = "Mock USD";
    string  public symbol = "mUSD";
    uint8   public decimals = 6;
    uint256 public totalSupply;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    bool public transferFromFails;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    function setTransferFromFails(bool v) external { transferFromFails = v; }

    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        totalSupply  += amount;
        emit Transfer(address(0), to, amount);
    }

    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount;
        emit Approval(msg.sender, spender, amount);
        return true;
    }

    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        if (transferFromFails) return false;
        uint256 a = allowance[from][msg.sender];
        require(a >= amount, "allowance");
        require(balanceOf[from] >= amount, "balance");
        unchecked {
            if (a != type(uint256).max) allowance[from][msg.sender] = a - amount;
            balanceOf[from] -= amount;
            balanceOf[to]   += amount;
        }
        emit Transfer(from, to, amount);
        return true;
    }
}
