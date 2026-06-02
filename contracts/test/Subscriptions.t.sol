// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Test.sol";
import {Subscriptions, IERC20} from "../src/Subscriptions.sol";
import {MockERC20} from "./MockERC20.sol";

contract SubscriptionsTest is Test {
    Subscriptions sub;
    MockERC20     token;

    address owner    = address(0xA11CE);
    address operator = address(0xB0B);
    address treasury = address(0xCAFE);
    address alice    = address(0xA1);
    address bob      = address(0xB2);

    uint256 constant PRICE = 10_000_000; // 10 USDC

    function setUp() public {
        token = new MockERC20();
        vm.prank(owner);
        sub = new Subscriptions(IERC20(address(token)), treasury, operator);
        token.mint(alice, 1000 * PRICE);
    }

    function test_constructor_rejectsZero() public {
        vm.expectRevert(Subscriptions.ZeroAddress.selector);
        new Subscriptions(IERC20(address(0)), treasury, operator);
        vm.expectRevert(Subscriptions.ZeroAddress.selector);
        new Subscriptions(IERC20(address(token)), address(0), operator);
        vm.expectRevert(Subscriptions.ZeroAddress.selector);
        new Subscriptions(IERC20(address(token)), treasury, address(0));
    }

    function test_constructor_setsImmutables() public view {
        assertEq(sub.owner(), owner);
        assertEq(sub.operator(), operator);
        assertEq(sub.treasury(), treasury);
        assertEq(address(sub.token()), address(token));
    }

    function test_pull_movesFunds() public {
        vm.prank(alice);
        token.approve(address(sub), 12 * PRICE);

        vm.expectEmit(true, false, false, true);
        emit Subscriptions.Charged(alice, PRICE);
        vm.prank(operator);
        sub.pull(alice, PRICE);

        assertEq(token.balanceOf(treasury), PRICE);
    }

    function test_pull_revertsNonOperator() public {
        vm.prank(alice);
        token.approve(address(sub), PRICE);
        vm.prank(bob);
        vm.expectRevert(Subscriptions.NotOperator.selector);
        sub.pull(alice, PRICE);
    }

    function test_pull_revertsOnInsufficientAllowance() public {
        vm.prank(operator);
        vm.expectRevert(); // MockERC20: "allowance"
        sub.pull(alice, PRICE);
    }

    function test_pull_revertsOnTransferFromFalse() public {
        vm.prank(alice);
        token.approve(address(sub), PRICE);
        token.setTransferFromFails(true);

        vm.prank(operator);
        vm.expectRevert(Subscriptions.TransferFailed.selector);
        sub.pull(alice, PRICE);
    }

    function test_pull_consumesAllowanceUpToCap() public {
        vm.prank(alice);
        token.approve(address(sub), 12 * PRICE);

        for (uint256 i = 0; i < 12; i++) {
            vm.prank(operator);
            sub.pull(alice, PRICE);
        }
        assertEq(token.balanceOf(treasury), 12 * PRICE);

        vm.prank(operator);
        vm.expectRevert(); // MockERC20: "allowance"
        sub.pull(alice, PRICE);
    }

    function test_setOperator_zeroHaltsPulls() public {
        // After setOperator(0), no address can pull (msg.sender can never be 0).
        vm.prank(owner);
        sub.setOperator(address(0));

        vm.prank(alice);
        token.approve(address(sub), PRICE);

        vm.prank(operator);
        vm.expectRevert(Subscriptions.NotOperator.selector);
        sub.pull(alice, PRICE);
    }

    function test_setOperator_onlyOwner() public {
        vm.prank(bob);
        vm.expectRevert(Subscriptions.NotOwner.selector);
        sub.setOperator(bob);

        vm.prank(owner);
        sub.setOperator(bob);
        assertEq(sub.operator(), bob);
    }

    function test_transferOwnership_movesPower() public {
        vm.prank(owner);
        sub.transferOwnership(bob);
        assertEq(sub.owner(), bob);

        vm.prank(owner);
        vm.expectRevert(Subscriptions.NotOwner.selector);
        sub.setOperator(bob);
    }

    function test_transferOwnership_rejectsZero() public {
        vm.prank(owner);
        vm.expectRevert(Subscriptions.ZeroAddress.selector);
        sub.transferOwnership(address(0));
    }
}
