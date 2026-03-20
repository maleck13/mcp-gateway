---
description: Book a restaurant and notify friends about the reservation
allowed-tools: mcp__mcp-gateway__discover_tools, AskUserQuestion
---

You are helping the user book a restaurant in their city and notify their friends about the reservation using mcp-gateway tools.

**IMPORTANT: You must follow these steps in order. Do not skip steps.**

## Step 1: List Tools

list all the mcp-gateway tools currently available to you in a markdown table with columns: **Tool Name** and **Description**. Output this to the user.

## step 2: Discover relevant tools 

 Use `mcp__mcp-gateway__discover_tools` with a relevant  query for listing and booking available restaurants, and finding friends and contacts so I can message them with an invite to the restaurant.  **End your turn with a prompt of  "ok I am ready to book you a restaurant. Would you like to see what restaurants are available? Let me know where and what time you are thinking of eating"**


On the next turn, before proceeding to Step 3, list all the mcp-gateway tools now currently available to you in a markdown table with columns: **Tool Name** and **Description**. This helps the user see what was discovered and what will be used.


## Step 3: List Restaurants

Use the discovered restaurant listing tool to get available restaurants in the user's chosen city. If the user hasn't chosen a city yet, show them a choice of cities first in a clear list.  

When a city is chosen present the list of available restaurants to the user in a clear format.

If no restaurants are found, inform the user and ask if they'd like to try a different city. Go back to Step 2.

## Step 4: Choose Restaurant

Ask the user which restaurant they'd like to book and at what time. Use AskUserQuestion to present the available restaurants as options.

## Step 5: Book the Restaurant

Use the discovered booking tool to reserve a table at the chosen restaurant in the chosen city.

If the booking fails, inform the user and offer to try a different restaurant. Go back to Step 4.

Confirm the booking to the user.

## Step 6: Get Friends List

Use the discovered friends/contacts tool to retrieve the user's contact list. Present the list to the user.

If the friends list is empty, inform the user that no contacts were found and skip to Step 9.

## Step 7: Choose Friends to Notify

Ask the user which friends they want to notify about the reservation. Use AskUserQuestion with multiSelect enabled so they can pick multiple friends.

## Step 8: Send Notification

Compose a friendly message about the reservation. The message should include:
- The restaurant name
- The city
- The time of the booking

Example: "Hey! I just booked a table at [restaurant] in [city] at [time]. Would love for you to join me!"

Use the discovered messaging tool to send this message to the selected friends.

If sending fails for any recipients, inform the user which sends failed.

## Step 9: Summary

Provide a summary of everything that was done:
- Restaurant booked (name and city)
- Friends notified (list of names)
- The message that was sent
