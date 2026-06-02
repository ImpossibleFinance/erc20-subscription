// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

interface IERC20 {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
}

/// @title  Subscription Puller
/// @notice The on-chain piece of a recurring-payment service. Users approve
///         this contract on the payment token; the operator pulls per plan
///         cadence. Plans, scheduling, cancel, dunning — all live off-chain.
///
/// User flow:
///   1. token.approve(this, plan.price * months)   — recommend ~12 months
///   2. tell the backend which plan to start
///   3. backend pulls plan.price each period via pull()
///
/// To cancel: hit the backend's cancel API (it stops scheduling), or
/// token.approve(this, 0) for a chain-level cutoff. Either works.
///
/// Emergency halt: owner calls setOperator(address(0)). No further pulls
/// possible until owner reassigns. (msg.sender can never equal 0, so the
/// onlyOperator modifier rejects every call.)
///
/// Trust model:
///   - owner    : rotates operator. Multisig on production.
///   - operator : backend's hot key. Compromise capped by each user's
///                outstanding USDC approval — keep that ~12 months max.
///   - treasury : immutable. Locked in bytecode so users can verify it once
///                and never have to re-trust.
contract Subscriptions {
    address public owner;
    address public operator;
    address public immutable treasury;
    IERC20  public immutable token;

    event OwnerChanged(address indexed owner);
    event OperatorChanged(address indexed operator);
    event Charged(address indexed user, uint256 amount);

    error NotOwner();
    error NotOperator();
    error ZeroAddress();
    error TransferFailed();

    modifier onlyOwner()    { if (msg.sender != owner)    revert NotOwner();    _; }
    modifier onlyOperator() { if (msg.sender != operator) revert NotOperator(); _; }

    constructor(IERC20 _token, address _treasury, address _operator) {
        if (address(_token) == address(0) || _treasury == address(0) || _operator == address(0)) {
            revert ZeroAddress();
        }
        owner    = msg.sender;
        token    = _token;
        treasury = _treasury;
        operator = _operator;
        emit OwnerChanged(msg.sender);
        emit OperatorChanged(_operator);
    }

    function transferOwnership(address a) external onlyOwner {
        if (a == address(0)) revert ZeroAddress();
        owner = a;
        emit OwnerChanged(a);
    }

    /// @notice Rotate (or halt with address(0)) the operator.
    function setOperator(address a) external onlyOwner {
        operator = a;
        emit OperatorChanged(a);
    }

    /// @notice Pull `amount` from `user` to the treasury. Operator-only.
    function pull(address user, uint256 amount) external onlyOperator {
        if (!token.transferFrom(user, treasury, amount)) revert TransferFailed();
        emit Charged(user, amount);
    }
}
