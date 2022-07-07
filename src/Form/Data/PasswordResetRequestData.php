<?php

namespace App\Form\Data;

use EWZ\Bundle\RecaptchaBundle\Validator\Constraints as ReCaptcha;
use Symfony\Component\Validator\Constraints as Assert;

class PasswordResetRequestData
{
    /**
     * @var string
     *
     * @Assert\Email()
     * @Assert\NotBlank()
     */
    public $email;

    /**
     * @var string
     *
     * @ReCaptcha\IsTrueV3()
     */
    public $recaptcha;
}
