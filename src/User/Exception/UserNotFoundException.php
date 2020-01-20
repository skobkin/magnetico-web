<?php

namespace App\User\Exception;

class UserNotFoundException extends \InvalidArgumentException
{
    protected $message = 'User not found';
}
