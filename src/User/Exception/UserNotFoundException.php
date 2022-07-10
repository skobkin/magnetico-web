<?php
declare(strict_types=1);

namespace App\User\Exception;

class UserNotFoundException extends \InvalidArgumentException
{
    protected $message = 'User not found';
}
